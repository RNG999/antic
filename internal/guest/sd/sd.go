// Package sd holds small helpers banken's commands share: systemd readiness
// notification and the BPF LSM check.
package sd

import (
	"errors"
	"net"
	"os"
	"strings"
)

// RequireBPFLSM fails unless "bpf" is an active LSM (lsm= boot parameter).
func RequireBPFLSM() error {
	b, err := os.ReadFile("/sys/kernel/security/lsm")
	if err != nil {
		return err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), ",") {
		if l == "bpf" {
			return nil
		}
	}
	return errors.New("BPF LSM is not active (add bpf to lsm=)")
}

// NotifyReady tells systemd (Type=notify) that the service is ready.
func NotifyReady() {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	if c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"}); err == nil {
		c.Write([]byte("READY=1"))
		c.Close()
	}
}
