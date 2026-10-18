package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// dialGuest connects to guest vsock port 22 through vhost-device-vsock's
// host socket ("hybrid vsock": send "CONNECT <port>", expect "OK <port>").
func dialGuest() (net.Conn, error) {
	c, err := net.DialTimeout("unix", vsockSock(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "CONNECT 22\n"); err != nil {
		c.Close()
		return nil, err
	}
	// Read the reply byte by byte so no SSH data is consumed.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, fmt.Errorf("vsock handshake: %w", err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !bytes.HasPrefix(line, []byte("OK ")) {
		c.Close()
		return nil, fmt.Errorf("vsock handshake: %q", line)
	}
	c.SetDeadline(time.Time{})
	return c, nil
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
	conn, err := dialGuest()
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(conn, "hanare", &ssh.ClientConfig{
		User:            guestUser(),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: pinHostKey,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

// pinHostKey accepts only the host key hanare generated for this guest
// (see ensureHostKey); there is no trust-on-first-use window.
func pinHostKey(_ string, _ net.Addr, key ssh.PublicKey) error {
	want, err := os.ReadFile(hostKeyPath() + ".pub")
	if err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(ssh.MarshalAuthorizedKey(key))) {
		return errors.New("guest host key does not match the one hanare generated")
	}
	return nil
}

// ensureHostKey creates the guest's SSH host key (passed to the guest as a
// credential at boot) if missing, or always when regenerate is set.
func ensureHostKey(regenerate bool) error {
	if _, err := os.Stat(hostKeyPath()); err == nil && !regenerate {
		return nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hanare guest host key")
	if err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(hostKeyPath()), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(hostKeyPath()+".pub", ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		return err
	}
	return os.WriteFile(hostKeyPath(), pem.EncodeToMemory(block), 0o600)
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
