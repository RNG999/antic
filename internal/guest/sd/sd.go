// Package sd notifies systemd (Type=notify) when a service is ready.
package sd

import (
	"net"
	"os"
)

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
