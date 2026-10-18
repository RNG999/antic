// Package transport connects to a guest: vsock through cloud-hypervisor's
// host socket, SSH sessions on top of it, and banken's agent.
package transport

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"
)

// Vsock dials guest vsock ports through cloud-hypervisor's "hybrid vsock"
// unix socket (send "CONNECT <port>", expect "OK <port>"). The socket lives
// in a 0700 directory, so other host users cannot reach the guest.
type Vsock struct{ Socket string }

func (v Vsock) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", v.Socket)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	// Read the reply byte by byte so no data after it is consumed.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, fmt.Errorf("vsock port %d: %w", port, err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !bytes.HasPrefix(line, []byte("OK ")) {
		c.Close()
		return nil, fmt.Errorf("vsock port %d: %q", port, line)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}
