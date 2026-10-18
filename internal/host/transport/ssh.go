package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// SSH runs commands in the guest as its login user.
type SSH struct {
	Vsock     Vsock
	User      string
	ClientKey string // private key file
	HostKey   string // the guest's host key (private; pinned via its public half)
}

// Cmd is one command line to run.
type Cmd struct {
	Line    string
	Offline bool             // use the offline sshd (no network)
	Stdin   io.Reader        // nil: no input
	Stdout  io.Writer        // nil: discard
	Stderr  io.Writer        // nil: discard
	PTY     *os.File         // terminal to use for a pty session (nil: none)
	Signals <-chan os.Signal // forwarded to the remote command
}

// ErrPermanent wraps errors retrying will not fix (keys, authentication).
var ErrPermanent = errors.New("permanent")

// Exec runs c and returns the remote exit status (128+N for signal N).
func (s SSH) Exec(ctx context.Context, c Cmd) (int, error) {
	client, err := s.dial(ctx, c.Offline)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go keepalive(ctx, client, cancel)

	if c.PTY != nil {
		fd := int(c.PTY.Fd())
		w, h, err := term.GetSize(fd)
		if err != nil {
			w, h = 80, 24
		}
		termName := os.Getenv("TERM")
		if termName == "" {
			termName = "xterm-256color"
		}
		if err := sess.RequestPty(termName, h, w, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
			return 0, err
		}
		if old, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, old)
		}
		winch := make(chan os.Signal, 1)
		notifyWinch(winch)
		defer stopNotify(winch)
		go func() {
			for {
				select {
				case <-winch:
					if w, h, err := term.GetSize(fd); err == nil {
						sess.WindowChange(h, w)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	sess.Stdout = orDiscard(c.Stdout)
	sess.Stderr = orDiscard(c.Stderr)
	if c.Stdin != nil {
		// Copy stdin ourselves: Session.Wait would otherwise block on a
		// stdin that never reaches EOF after the command has exited.
		in, err := sess.StdinPipe()
		if err != nil {
			return 0, err
		}
		go func() {
			io.Copy(in, c.Stdin)
			in.Close()
		}()
	}
	if err := sess.Start(c.Line); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	signals := 0
	for {
		select {
		case err := <-done:
			return exitStatus(err)
		case sig := <-c.Signals:
			signals++
			if signals > 1 {
				// A second interrupt: stop waiting for the remote side.
				return 128 + signalNumber(sig), nil
			}
			sess.Signal(sshSignal(sig))
		case <-ctx.Done():
			return 255, fmt.Errorf("connection to the guest lost")
		}
	}
}

// Ready reports whether an SSH session can be established now.
func (s SSH) Ready(ctx context.Context) error {
	c, err := s.dial(ctx, false)
	if err != nil {
		return err
	}
	return c.Close()
}

func (s SSH) dial(ctx context.Context, offline bool) (*ssh.Client, error) {
	key, err := os.ReadFile(s.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("%w: client key: %w", ErrPermanent, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("%w: client key %s: %w", ErrPermanent, s.ClientKey, err)
	}
	pin, err := PinnedHostKey(s.HostKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPermanent, err)
	}
	port := uint32(22)
	if offline {
		port = 23
	}
	conn, err := s.Vsock.Dial(ctx, port)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(conn, "hanare", &ssh.ClientConfig{
		User: s.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if string(k.Marshal()) != string(pin.Marshal()) {
				return fmt.Errorf("%w: guest host key does not match the one hanare generated", ErrPermanent)
			}
			return nil
		},
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

// keepalive cancels the session when the guest stops answering.
func keepalive(ctx context.Context, c *ssh.Client, cancel context.CancelFunc) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok := make(chan bool, 1)
			go func() {
				_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
				ok <- err == nil
			}()
			select {
			case good := <-ok:
				if good {
					misses = 0
					continue
				}
			case <-time.After(5 * time.Second):
			}
			if misses++; misses >= 3 {
				cancel()
				return
			}
		}
	}
}

func exitStatus(err error) (int, error) {
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		if sig := exitErr.Signal(); sig != "" {
			if n := unix.SignalNum("SIG" + sig); n != 0 {
				return 128 + int(n), nil
			}
			return 255, nil
		}
		return exitErr.ExitStatus(), nil
	}
	return 255, err
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// EnsureHostKey creates the guest's host key at path if missing (always, if
// regenerate). Only the private key is stored; the pin derives from it.
func EnsureHostKey(path string, regenerate bool) error {
	if _, err := os.Stat(path); err == nil && !regenerate {
		return nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hanare guest host key")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".host_key.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(pem.EncodeToMemory(block)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// PinnedHostKey returns the public key of the host key at path.
func PinnedHostKey(path string) (ssh.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("guest host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("guest host key %s: %w", path, err)
	}
	return signer.PublicKey(), nil
}
