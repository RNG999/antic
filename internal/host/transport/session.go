package transport

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/RNG999/antic/internal/proto"
)

// Cmd is one command or interactive shell session to run in the guest.
type Cmd struct {
	Argv    []string         // empty: interactive login shell
	Dir     string           // working directory in the guest
	Offline bool             // use the offline session unit (no network)
	Stdin   io.Reader        // nil: no input
	Stdout  io.Writer        // nil: discard
	Stderr  io.Writer        // nil: discard
	PTY     *os.File         // terminal to use for a pty session (nil: none)
	Signals <-chan os.Signal // forwarded to the remote command
}

// Exec runs c in the guest and returns the remote exit status (128+N for signal N).
func (v Vsock) Exec(ctx context.Context, c Cmd) (int, error) {
	port := uint32(proto.PortSession)
	if c.Offline {
		port = proto.PortSessionOffline
	}
	conn, err := v.Dial(ctx, port)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	req := proto.SessionReq{
		Argv: c.Argv,
		Dir:  c.Dir,
	}
	if termName := os.Getenv("TERM"); termName != "" {
		req.Env = append(req.Env, "TERM="+termName)
	} else if c.PTY != nil {
		req.Env = append(req.Env, "TERM=xterm-256color")
	}

	if c.PTY != nil {
		fd := int(c.PTY.Fd())
		w, h, err := term.GetSize(fd)
		if err != nil {
			w, h = 80, 24
		}
		req.PTY = true
		req.Rows = uint16(h)
		req.Cols = uint16(w)
		if old, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, old)
		}
	}

	b, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return 0, err
	}
	conn.SetWriteDeadline(time.Time{})

	fw := &frameWriter{w: conn}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.PTY != nil {
		fd := int(c.PTY.Fd())
		winch := make(chan os.Signal, 1)
		notifyWinch(winch)
		defer stopNotify(winch)
		go func() {
			for {
				select {
				case <-winch:
					if w, h, err := term.GetSize(fd); err == nil {
						var sz [4]byte
						binary.BigEndian.PutUint16(sz[0:2], uint16(h))
						binary.BigEndian.PutUint16(sz[2:4], uint16(w))
						_ = fw.write(proto.FrameResize, sz[:])
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	if c.Stdin != nil {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := c.Stdin.Read(buf)
				if n > 0 {
					if werr := fw.write(proto.FrameStdin, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					_ = fw.write(proto.FrameCloseStdin, nil)
					return
				}
			}
		}()
	} else if !req.PTY {
		_ = fw.write(proto.FrameCloseStdin, nil)
	}

	var lastPong atomic.Int64
	lastPong.Store(time.Now().UnixNano())
	go keepalive(ctx, fw, &lastPong, cancel)

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := readLoop(bufio.NewReader(conn), orDiscard(c.Stdout), orDiscard(c.Stderr), &lastPong)
		done <- result{code, err}
	}()

	signals := 0
	for {
		select {
		case res := <-done:
			return res.code, res.err
		case sig := <-c.Signals:
			signals++
			if signals > 1 {
				return 128 + signalNumber(sig), nil
			}
			_ = fw.write(proto.FrameSignal, []byte{byte(signalNumber(sig))})
		case <-ctx.Done():
			return 255, fmt.Errorf("connection to the guest lost")
		}
	}
}

func readLoop(r io.Reader, stdout, stderr io.Writer, lastPong *atomic.Int64) (int, error) {
	hdr := make([]byte, 5)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return 255, fmt.Errorf("connection to the guest closed unexpectedly")
			}
			return 255, err
		}
		lastPong.Store(time.Now().UnixNano())
		typ := hdr[0]
		n := binary.BigEndian.Uint32(hdr[1:5])
		var payload []byte
		if n > 0 {
			payload = make([]byte, n)
			if _, err := io.ReadFull(r, payload); err != nil {
				return 255, err
			}
		}
		switch typ {
		case proto.FrameStdout:
			_, _ = stdout.Write(payload)
		case proto.FrameStderr:
			_, _ = stderr.Write(payload)
		case proto.FrameError:
			return 125, errors.New(string(payload))
		case proto.FrameExit:
			if len(payload) != 4 {
				return 255, fmt.Errorf("invalid exit frame")
			}
			return int(int32(binary.BigEndian.Uint32(payload))), nil
		case proto.FramePong:
		}
	}
}

func keepalive(ctx context.Context, fw *frameWriter, lastPong *atomic.Int64, cancel context.CancelFunc) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(0, lastPong.Load())) > 20*time.Second {
				cancel()
				return
			}
			if err := fw.write(proto.FramePing, nil); err != nil {
				cancel()
				return
			}
		}
	}
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) write(typ byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := f.w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := f.w.Write(payload)
		return err
	}
	return nil
}
