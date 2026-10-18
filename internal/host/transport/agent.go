package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/RNG999/hanare/internal/proto"
)

// Agent sends one request to banken's agent and passes each response to
// fn until the last one. Only the host can reach the agent (it checks the
// peer CID), so this is hanare's root channel into the guest.
func (v Vsock) Agent(ctx context.Context, req proto.Request, fn func(proto.Response)) error {
	c, err := v.Dial(ctx, proto.PortAgent)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	dec := json.NewDecoder(bufio.NewReader(c))
	for {
		var r proto.Response
		if err := dec.Decode(&r); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("agent %s: %w", req.Op, err)
		}
		if r.Error != "" {
			return fmt.Errorf("agent %s: %s", req.Op, r.Error)
		}
		if fn != nil {
			fn(r)
		}
		if r.Done {
			return nil
		}
	}
}

// Hello asks the agent for its protocol version, user and readiness.
func (v Vsock) Hello(ctx context.Context) (proto.Hello, error) {
	var h *proto.Hello
	err := v.Agent(ctx, proto.Request{Op: proto.OpHello}, func(r proto.Response) {
		if r.Hello != nil {
			h = r.Hello
		}
	})
	if err == nil && h == nil {
		err = errors.New("agent hello: no answer")
	}
	if err != nil {
		return proto.Hello{}, err
	}
	return *h, nil
}
