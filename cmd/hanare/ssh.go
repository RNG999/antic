package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// vsockConn adapts a connected AF_VSOCK socket to net.Conn; *os.File already
// provides Read/Write/Close and deadlines for non-blocking descriptors.
type vsockConn struct{ *os.File }

type vsockAddr struct{}

func (vsockAddr) Network() string      { return "vsock" }
func (vsockAddr) String() string       { return "vsock" }
func (vsockConn) LocalAddr() net.Addr  { return vsockAddr{} }
func (vsockConn) RemoteAddr() net.Addr { return vsockAddr{} }

func dialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return vsockConn{os.NewFile(uintptr(fd), "vsock")}, nil
}

// dial opens an SSH connection to the guest over vsock port 22.
func (c *Config) dial() (*ssh.Client, error) {
	key, err := os.ReadFile(keyPath())
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, err
	}
	conn, err := dialVsock(c.CID, 22)
	if err != nil {
		return nil, err
	}
	conn.(vsockConn).SetDeadline(time.Now().Add(10 * time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(conn, "hanare", &ssh.ClientConfig{
		User:            c.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: pinHostKey,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.(vsockConn).SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

// pinHostKey trusts the guest's host key on first use; "hanare rebuild"
// forgets it because a fresh image generates new keys.
func pinHostKey(_ string, _ net.Addr, key ssh.PublicKey) error {
	got := ssh.MarshalAuthorizedKey(key)
	want, err := os.ReadFile(hostKeyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return os.WriteFile(hostKeyPath(), got, 0o600)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		return fmt.Errorf("guest host key changed (delete %s if the image was rebuilt)", hostKeyPath())
	}
	return nil
}

// connect dials the guest, starting the VM first if it is not running.
func (c *Config) connect() (*ssh.Client, error) {
	if client, err := c.dial(); err == nil {
		return client, nil
	}
	if !unitActive() {
		fmt.Fprintln(os.Stderr, "hanare: starting VM...")
		if err := startUnit(); err != nil {
			return nil, err
		}
	}
	return c.waitReady(120 * time.Second)
}

func (c *Config) waitReady(timeout time.Duration) (*ssh.Client, error) {
	deadline := time.Now().Add(timeout)
	for {
		client, err := c.dial()
		if err == nil {
			return client, nil
		}
		if !unitActive() {
			return nil, fmt.Errorf("VM exited (see: hanare logs, journalctl --user -u %s)", unit)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("VM not reachable over SSH: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

var forwardedSignals = map[os.Signal]ssh.Signal{
	syscall.SIGINT:  ssh.SIGINT,
	syscall.SIGTERM: ssh.SIGTERM,
	syscall.SIGHUP:  ssh.SIGHUP,
	syscall.SIGQUIT: ssh.SIGQUIT,
}

// remote runs a shell command line in the guest and returns its exit status
// (128+N for signal N, 255 for connection failures). A pty is used when
// stdin and stdout are terminals, or always when forcePty is set.
func (c *Config) remote(cmdline string, forcePty bool) int {
	client, err := c.connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 255
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 255
	}
	defer sess.Close()

	stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	usePty := term.IsTerminal(stdinFd) && (term.IsTerminal(stdoutFd) || forcePty)
	if usePty {
		w, h, err := term.GetSize(stdinFd)
		if err != nil {
			w, h = 80, 24
		}
		termName := os.Getenv("TERM")
		if termName == "" {
			termName = "xterm-256color"
		}
		if err := sess.RequestPty(termName, h, w, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
			fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
			return 255
		}
		old, err := term.MakeRaw(stdinFd)
		if err == nil {
			defer term.Restore(stdinFd, old)
		}
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(stdinFd); err == nil {
					sess.WindowChange(h, w)
				}
			}
		}()
	}

	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	// Copy stdin ourselves: Session.Wait would otherwise block on a stdin
	// that never reaches EOF (e.g. a terminal) after the command has exited.
	stdin, err := sess.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 255
	}
	go func() {
		io.Copy(stdin, os.Stdin)
		stdin.Close()
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			sess.Signal(forwardedSignals[s])
		}
	}()

	if err := sess.Start(cmdline); err != nil {
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 255
	}
	err = sess.Wait()
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		if sig := exitErr.Signal(); sig != "" {
			if n := unix.SignalNum("SIG" + sig); n != 0 {
				return 128 + int(n)
			}
			return 255
		}
		return exitErr.ExitStatus()
	default:
		fmt.Fprintf(os.Stderr, "hanare: %v\n", err)
		return 255
	}
}
