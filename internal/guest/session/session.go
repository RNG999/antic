// Package session runs a single command or interactive shell inside a
// systemd-sandboxed session unit (antic-session@.service), speaking proto's
// framed session protocol on the socket-activated vsock connection (fd 0/1).
package session

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/RNG999/antic/internal/proto"
)

// Run serves one socket-activated session on stdin/stdout (fd 0/1).
func Run() error {
	sa, err := unix.Getpeername(0)
	if err != nil {
		return fmt.Errorf("getpeername: %w", err)
	}
	if vm, ok := sa.(*unix.SockaddrVM); !ok || vm.CID != proto.HostCID {
		return fmt.Errorf("refused session connection from %v", sa)
	}

	br := bufio.NewReader(os.Stdin)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read session request: %w", err)
	}
	var req proto.SessionReq
	if err := json.Unmarshal(line, &req); err != nil {
		return fmt.Errorf("decode session request: %w", err)
	}

	fw := &frameWriter{w: os.Stdout}
	if err := runCmd(br, fw, req); err != nil {
		_ = fw.write(proto.FrameError, []byte(err.Error()))
		return err
	}
	return nil
}

func runCmd(r io.Reader, fw *frameWriter, req proto.SessionReq) error {
	dir := req.Dir
	if dir == "" || dir == "~" {
		dir = os.Getenv("HOME")
	}
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			return fmt.Errorf("chdir %s: %w", dir, err)
		}
	}

	var cmd *exec.Cmd
	if len(req.Argv) == 0 {
		sh := findShell()
		cmd = exec.Command(sh, "-l")
		cmd.Args[0] = "-" + filepath.Base(sh)
	} else {
		cmd = exec.Command(req.Argv[0], req.Argv[1:]...)
	}
	cmd.Env = append(os.Environ(), req.Env...)

	var (
		master *os.File
		stdinW io.WriteCloser
		outWG  sync.WaitGroup
	)

	if req.PTY {
		var slave *os.File
		var err error
		master, slave, err = openPTY(req.Rows, req.Cols)
		if err != nil {
			return fmt.Errorf("open pty: %w", err)
		}
		defer master.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
		if err := cmd.Start(); err != nil {
			slave.Close()
			return err
		}
		slave.Close()
		stdinW = master
		outWG.Add(1)
		go func() {
			defer outWG.Done()
			pumpOut(master, fw, proto.FrameStdout)
		}()
	} else {
		var err error
		stdinW, err = cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdoutR, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		stderrR, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		outWG.Add(2)
		go func() {
			defer outWG.Done()
			pumpOut(stdoutR, fw, proto.FrameStdout)
		}()
		go func() {
			defer outWG.Done()
			pumpOut(stderrR, fw, proto.FrameStderr)
		}()
	}

	// Read frames from the host (stdin, resize, signals, keepalive).
	go func() {
		hdr := make([]byte, 5)
		for {
			if _, err := io.ReadFull(r, hdr); err != nil {
				if !req.PTY && stdinW != nil {
					stdinW.Close()
				}
				if cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
				}
				return
			}
			typ := hdr[0]
			n := binary.BigEndian.Uint32(hdr[1:5])
			var payload []byte
			if n > 0 {
				payload = make([]byte, n)
				if _, err := io.ReadFull(r, payload); err != nil {
					return
				}
			}
			switch typ {
			case proto.FrameStdin:
				if stdinW != nil && len(payload) > 0 {
					_, _ = stdinW.Write(payload)
				}
			case proto.FrameCloseStdin:
				if !req.PTY && stdinW != nil {
					_ = stdinW.Close()
					stdinW = nil
				}
			case proto.FrameResize:
				if master != nil && len(payload) == 4 {
					rows := binary.BigEndian.Uint16(payload[0:2])
					cols := binary.BigEndian.Uint16(payload[2:4])
					_ = unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
				}
			case proto.FrameSignal:
				if cmd.Process != nil && len(payload) == 1 {
					sig := syscall.Signal(payload[0])
					_ = syscall.Kill(-cmd.Process.Pid, sig)
				}
			case proto.FramePing:
				_ = fw.write(proto.FramePong, nil)
			}
		}
	}()

	err := cmd.Wait()
	if master != nil {
		_ = master.Close()
	}
	outWG.Wait()

	var code [4]byte
	binary.BigEndian.PutUint32(code[:], uint32(exitStatus(err)))
	_ = fw.write(proto.FrameExit, code[:])
	return nil
}

func findShell() string {
	for _, s := range []string{os.Getenv("SHELL"), "/run/current-system/sw/bin/bash", "/run/current-system/sw/bin/sh", "/bin/sh"} {
		if s != "" {
			if _, err := os.Stat(s); err == nil {
				return s
			}
		}
	}
	return "/bin/sh"
}

func openPTY(rows, cols uint16) (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(mfd)
		return nil, nil, err
	}
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	_ = unix.IoctlSetWinsize(mfd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	r1, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), uintptr(unix.TIOCGPTPEER), uintptr(unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC))
	sfd := int(r1)
	if errno != 0 {
		n, gerr := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
		if gerr != nil {
			unix.Close(mfd)
			return nil, nil, errno
		}
		var oerr error
		sfd, oerr = unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if oerr != nil {
			unix.Close(mfd)
			return nil, nil, oerr
		}
	}
	return os.NewFile(uintptr(mfd), "ptmx"), os.NewFile(uintptr(sfd), "pts"), nil
}

func pumpOut(r io.Reader, fw *frameWriter, typ byte) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := fw.write(typ, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 255
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
