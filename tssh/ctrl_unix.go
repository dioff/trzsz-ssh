//go:build !windows

/*
MIT License

Copyright (c) 2023 Lonny Wong <lonnywong@qq.com>
Copyright (c) 2023 [Contributors](https://github.com/trzsz/trzsz-ssh/graphs/contributors)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package tssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

type controlMaster struct {
	path      string
	args      []string
	cmd       *exec.Cmd
	ptmx      *os.File
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	loggingIn atomic.Bool
	exited    atomic.Bool
}

func (c *controlMaster) handleStderr() {
	go func() {
		defer c.stderr.Close()
		buf := make([]byte, 100)
		for c.loggingIn.Load() {
			n, err := c.stderr.Read(buf)
			if n > 0 {
				fmt.Fprintf(os.Stderr, "%s", string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()
}

func (c *controlMaster) handleStdout() <-chan error {
	doneCh := make(chan error, 1)
	go func() {
		defer close(doneCh)
		buf := make([]byte, 1000)
		n, err := c.stdout.Read(buf)
		if err != nil {
			doneCh <- fmt.Errorf("read stdout failed: %v", err)
			return
		}
		if !bytes.Equal(bytes.TrimSpace(buf[:n]), []byte("ok")) {
			doneCh <- fmt.Errorf("control master stdout invalid: %v", buf[:n])
			return
		}
		doneCh <- nil
	}()
	return doneCh
}

func (c *controlMaster) fillPassword(args *sshArgs, expectCount uint32) (cancel context.CancelFunc) {
	var ctx context.Context
	if expectTimeout := getExpectTimeout(args, "Ctrl"); expectTimeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(expectTimeout)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}

	expect := &sshExpect{
		ctx: ctx,
		pre: "Ctrl",
		out: make(chan []byte, 1),
	}
	go expect.wrapOutput(c.ptmx, nil, expect.out)
	go expect.execInteractions(args.Destination, c.ptmx, expectCount)
	return
}

func (c *controlMaster) checkExit() <-chan struct{} {
	exitCh := make(chan struct{}, 1)
	go func() {
		defer close(exitCh)
		_ = c.cmd.Wait()
		c.exited.Store(true)
		if c.ptmx != nil {
			c.ptmx.Close()
		}
		exitCh <- struct{}{}
	}()
	return exitCh
}

func (c *controlMaster) start(args *sshArgs) error {
	var err error
	c.cmd = exec.Command(c.path, c.args...)
	expectCount := getExpectCount(args, "Ctrl")
	if expectCount > 0 {
		c.cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
		}
		pty, tty, err := pty.Open()
		if err != nil {
			return fmt.Errorf("open pty failed: %v", err)
		}
		defer tty.Close()
		c.cmd.Stdin = tty
		c.ptmx = pty
		cancel := c.fillPassword(args, expectCount)
		defer cancel()
	}
	if c.stdout, err = c.cmd.StdoutPipe(); err != nil {
		return fmt.Errorf("stdout pipe failed: %v", err)
	}
	if c.stderr, err = c.cmd.StderrPipe(); err != nil {
		return fmt.Errorf("stderr pipe failed: %v", err)
	}
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("control master start failed: %v", err)
	}

	c.loggingIn.Store(true)

	intCh := make(chan os.Signal, 1)
	signal.Notify(intCh, os.Interrupt)
	defer func() { signal.Stop(intCh); close(intCh) }()

	c.handleStderr()
	exitCh := c.checkExit()
	doneCh := c.handleStdout()

	defer func() {
		c.loggingIn.Store(false)
		if !c.exited.Load() {
			onExitFuncs = append(onExitFuncs, func() {
				c.quit(exitCh)
			})
		}
	}()

	for {
		select {
		case err := <-doneCh:
			return err
		case <-exitCh:
			return fmt.Errorf("control master process exited")
		case <-intCh:
			c.quit(exitCh)
			return fmt.Errorf("user interrupt control master")
		}
	}
}

func (c *controlMaster) quit(exit <-chan struct{}) {
	if c.exited.Load() {
		return
	}
	_ = c.cmd.Process.Signal(syscall.SIGINT)
	timer := time.AfterFunc(500*time.Millisecond, func() {
		_ = c.cmd.Process.Kill()
	})
	<-exit
	timer.Stop()
}

func getRealPath(path string) string {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return realPath
}

func getOpenSSH() (string, error) {
	sshPath := "/usr/bin/ssh"
	tsshPath, err := os.Executable()
	if err != nil {
		return "", err
	}
	if getRealPath(tsshPath) == getRealPath(sshPath) {
		return "", fmt.Errorf("%s is the current program", sshPath)
	}
	return sshPath, nil
}

func startControlMaster(args *sshArgs) error {
	sshPath, err := getOpenSSH()
	if err != nil {
		return fmt.Errorf("can't find openssh program: %v", err)
	}

	cmdArgs := []string{"-T", "-oRemoteCommand=none", "-oConnectTimeout=5"}

	if args.Debug {
		cmdArgs = append(cmdArgs, "-v")
	}
	if !args.NoForwardAgent && args.ForwardAgent {
		cmdArgs = append(cmdArgs, "-A")
	}
	if args.LoginName != "" {
		cmdArgs = append(cmdArgs, "-l", args.LoginName)
	}
	if args.Port != 0 {
		cmdArgs = append(cmdArgs, "-p", strconv.Itoa(args.Port))
	}
	if args.ConfigFile != "" {
		cmdArgs = append(cmdArgs, "-F", args.ConfigFile)
	}
	if args.ProxyJump != "" {
		cmdArgs = append(cmdArgs, "-J", args.ProxyJump)
	}

	for _, identity := range args.Identity.values {
		cmdArgs = append(cmdArgs, "-i", identity)
	}
	for _, b := range args.DynamicForward.binds {
		cmdArgs = append(cmdArgs, "-D", b.argument)
	}
	for _, f := range args.LocalForward.cfgs {
		cmdArgs = append(cmdArgs, "-L", f.argument)
	}
	for _, f := range args.RemoteForward.cfgs {
		cmdArgs = append(cmdArgs, "-R", f.argument)
	}

	for key, values := range args.Option.options {
		switch key {
		case "remotecommand":
			break
		case "enabletrzsz", "enabledragfile":
			break
		default:
			for _, value := range values {
				cmdArgs = append(cmdArgs, fmt.Sprintf("-o%s=%s", key, value))
			}
		}
	}

	if args.originalDest != "" {
		cmdArgs = append(cmdArgs, args.originalDest)
	} else {
		cmdArgs = append(cmdArgs, args.Destination)
	}
	// 10 seconds is enough for tssh to connect
	cmdArgs = append(cmdArgs, "echo ok; sleep 10")

	if enableDebugLogging {
		debug("control master: %s %s", sshPath, strings.Join(cmdArgs, " "))
	}

	ctrlMaster := &controlMaster{path: sshPath, args: cmdArgs}
	if err := ctrlMaster.start(args); err != nil {
		return err
	}
	debug("start control master success")
	return nil
}

func connectViaControl(args *sshArgs, param *loginParam) *ssh.Client {
	ctrlMaster := getOptionConfig(args, "ControlMaster")
	ctrlPath := getOptionConfig(args, "ControlPath")

	switch strings.ToLower(ctrlPath) {
	case "", "none":
		return nil
	}

	socket := resolveHomeDir(expandTokens(ctrlPath, args, param, "%CdhikLlnpru"))

	switch strings.ToLower(ctrlMaster) {
	case "yes", "ask":
		if isFileExist(socket) {
			warning("control socket [%s] already exists, disabling multiplexing", socket)
			return nil
		}
		fallthrough
	case "auto", "autoask":
		if err := startControlMaster(args); err != nil {
			warning("start control master failed: %v", err)
		}
	}

	debug("login to [%s], socket: %s", args.Destination, socket)

	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		warning("dial control socket [%s] failed: %v", socket, err)
		return nil
	}

	ncc, chans, reqs, err := NewControlClientConn(conn)
	if err != nil {
		warning("new conn from control socket [%s] failed: %v", socket, err)
		return nil
	}

	debug("login to [%s] success", args.Destination)
	return ssh.NewClient(ncc, chans, reqs)
}
