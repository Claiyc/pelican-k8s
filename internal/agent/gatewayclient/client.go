// Package gatewayclient talks to the pelican-k8s gateway's agent-only
// extensions of the Wings remote API (install coordination). Everything else
// the agent needs from the gateway goes through Wings' own remote client.
package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is an authenticated HTTP client for the gateway extensions.
type Client struct {
	base    string
	tokenID string
	token   string
	http    *http.Client
}

// New returns a client for the gateway at base (the Wings `remote` URL).
func New(base, tokenID, token string) *Client {
	return &Client{base: strings.TrimSuffix(base, "/") + "/api/remote", tokenID: tokenID, token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

// PreparedResponse is returned once the gateway recorded the prepared generation.
type PreparedResponse struct {
	Generation     int64 `json:"generation"`
	StrictExitCode bool  `json:"strict_exit_code"`
}

// InstallState is the gateway's view of the current install.
type InstallState struct {
	Generation  int64  `json:"generation"`
	Result      string `json:"result"`
	JobFinished bool   `json:"job_finished"`
}

// InstallPrepared reports that the agent holds the install lock and returns the
// generation it should wait for.
func (c *Client) InstallPrepared(ctx context.Context, uuid string) (*PreparedResponse, error) {
	var out PreparedResponse
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/servers/%s/install/prepared", uuid), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetInstallState returns the state of the install for the generation.
func (c *Client) GetInstallState(ctx context.Context, uuid string, generation int64) (*InstallState, error) {
	var out InstallState
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/servers/%s/install/state?generation=%d", uuid, generation), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
	req.Header.Set("Authorization", "Bearer "+c.tokenID+"."+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("gateway %s %s: HTTP %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
