// Package chv is a client for cloud-hypervisor's REST API on its unix
// socket. The VM configuration (including credentials) goes through the
// API, never the command line, which other host processes can read.
package chv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to one cloud-hypervisor process.
type Client struct{ hc *http.Client }

// New returns a client for the API socket at path.
func New(path string) *Client {
	return &Client{hc: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}}
}

// VMConfig is the subset of cloud-hypervisor's VmConfig antic uses.
type VMConfig struct {
	Payload  Payload   `json:"payload"`
	CPUs     CPUs      `json:"cpus"`
	Memory   Memory    `json:"memory"`
	FS       []FS      `json:"fs,omitempty"`
	Net      []Net     `json:"net,omitempty"`
	Vsock    *Vsock    `json:"vsock,omitempty"`
	Serial   Console   `json:"serial"`
	Console  Console   `json:"console"`
	Platform *Platform `json:"platform,omitempty"`
	RNG      *RNG      `json:"rng,omitempty"`
	Balloon  *struct{} `json:"balloon,omitempty"`
	Watchdog bool      `json:"watchdog"`
}

type Payload struct {
	Kernel    string `json:"kernel"`
	Initramfs string `json:"initramfs,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
}

type CPUs struct {
	Boot int `json:"boot_vcpus"`
	Max  int `json:"max_vcpus"`
}

type Memory struct {
	Size   int64 `json:"size"`
	Shared bool  `json:"shared"` // required by vhost-user devices
}

type FS struct {
	ID        string `json:"id,omitempty"`
	Tag       string `json:"tag"`
	Socket    string `json:"socket"`
	NumQueues int    `json:"num_queues"`
	QueueSize int    `json:"queue_size"`
}

type Net struct {
	ID          string `json:"id,omitempty"`
	VhostUser   bool   `json:"vhost_user"`
	VhostSocket string `json:"vhost_socket"`
	VhostMode   string `json:"vhost_mode"`
	NumQueues   int    `json:"num_queues"`
	QueueSize   int    `json:"queue_size"`
}

type Vsock struct {
	ID     string `json:"id,omitempty"`
	CID    uint32 `json:"cid"`
	Socket string `json:"socket"`
}

type Console struct {
	Mode string `json:"mode"` // "Off", "File", ...
	File string `json:"file,omitempty"`
}

type Platform struct {
	OEMStrings []string `json:"oem_strings,omitempty"`
}

type RNG struct {
	Src string `json:"src"`
}

// Info is the part of vm.info antic reads.
type Info struct {
	State string `json:"state"` // Created, Running, Shutdown, Paused
}

func (c *Client) Create(ctx context.Context, cfg VMConfig) error {
	return c.put(ctx, "vm.create", cfg, nil)
}
func (c *Client) Boot(ctx context.Context) error { return c.put(ctx, "vm.boot", nil, nil) }
func (c *Client) PowerButton(ctx context.Context) error {
	return c.put(ctx, "vm.power-button", nil, nil)
}
func (c *Client) AddFS(ctx context.Context, fs FS) error {
	return c.put(ctx, "vm.add-fs", fs, nil)
}
func (c *Client) RemoveDevice(ctx context.Context, id string) error {
	return c.put(ctx, "vm.remove-device", map[string]string{"id": id}, nil)
}
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.do(ctx, http.MethodGet, "vm.info", nil, &i)
	return i, err
}

func (c *Client) put(ctx context.Context, ep string, body, out any) error {
	return c.do(ctx, http.MethodPut, ep, body, out)
}

func (c *Client) do(ctx context.Context, method, ep string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/v1/"+ep, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cloud-hypervisor %s: %w", ep, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cloud-hypervisor %s: %s: %s", ep, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}
