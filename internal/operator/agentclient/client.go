// Package agentclient drives an agent's Wings HTTP API with the agent token.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one agent.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the agent at base (http://<pod ip>:8080).
func New(base, token string) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), token: token, http: &http.Client{Timeout: 20 * time.Second}}
}

// ErrConflict is returned for HTTP 409 (e.g. reinstall during a power action).
var ErrConflict = errors.New("agent: conflict")

// ErrUnavailable is returned when the agent cannot be reached.
var ErrUnavailable = errors.New("agent: unavailable")

// State is the agent's view of the server.
type State struct {
	State       string `json:"state"`
	IsSuspended bool   `json:"is_suspended"`
	Utilization struct {
		MemoryBytes uint64  `json:"memory_bytes"`
		CPUAbsolute float64 `json:"cpu_absolute"`
		DiskBytes   uint64  `json:"disk_bytes"`
		Uptime      int64   `json:"uptime"`
	} `json:"utilization"`
}

// Healthy reports whether /internal/v1/healthz answers.
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/internal/v1/healthz", nil)
	if err != nil {
		return false
	}
	res, err := c.http.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// GetServer returns the server state.
func (c *Client) GetServer(ctx context.Context, uuid string) (*State, error) {
	var st State
	if err := c.do(ctx, http.MethodGet, "/api/servers/"+uuid, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Power issues a power action (start, stop, restart, kill).
func (c *Client) Power(ctx context.Context, uuid, action string) error {
	return c.do(ctx, http.MethodPost, "/api/servers/"+uuid+"/power", map[string]any{"action": action}, nil)
}

// Sync asks the agent to re-fetch its configuration from the gateway.
func (c *Client) Sync(ctx context.Context, uuid string) error {
	return c.do(ctx, http.MethodPost, "/api/servers/"+uuid+"/sync", nil, nil)
}

// Install starts an installation.
func (c *Client) Install(ctx context.Context, uuid string, reinstall bool) error {
	path := "/api/servers/" + uuid + "/install"
	if reinstall {
		path = "/api/servers/" + uuid + "/reinstall"
	}
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// Delete tells Wings to destroy the server (kills the process and removes files).
func (c *Client) Delete(ctx context.Context, uuid string) error {
	return c.do(ctx, http.MethodDelete, "/api/servers/"+uuid, nil, nil)
}

// ExitState injects a process exit observed from the container status.
func (c *Client) ExitState(ctx context.Context, code int32, oomKilled bool) error {
	return c.do(ctx, http.MethodPost, "/internal/v1/exit-state", map[string]any{"code": code, "oomKilled": oomKilled}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		return ErrConflict
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("agent %s %s: HTTP %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}
