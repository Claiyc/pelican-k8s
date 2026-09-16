// Package panel is the gateway's client for the Panel remote API, using the
// node token exactly like Wings' remote client does.
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pelican/wings/remote"
)

// Client calls <panel>/api/remote with the node credentials.
type Client struct {
	base      string
	tokenID   string
	token     string
	userAgent string
	http      *http.Client
}

// New returns a Panel client.
func New(panelURL, tokenID, token, userAgent string) *Client {
	return &Client{
		base:      strings.TrimSuffix(panelURL, "/") + "/api/remote",
		tokenID:   tokenID,
		token:     token,
		userAgent: userAgent,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// Error is a non-2xx Panel response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("panel: HTTP %d: %s", e.Status, e.Body) }

// IsNotFound reports whether err is a 404 from the Panel.
func IsNotFound(err error) bool {
	e, ok := err.(*Error)
	return ok && e.Status == http.StatusNotFound
}

// Do performs a request and returns the status code and body. Bodies are
// forwarded verbatim, so agent posts can be relayed without re-encoding.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, query map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if query != nil {
		q := req.URL.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("Authorization", "Bearer "+c.tokenID+"."+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, b, nil
}

func (c *Client) getJSON(ctx context.Context, path string, query map[string]string, out any) error {
	status, body, err := c.Do(ctx, http.MethodGet, path, nil, query)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &Error{Status: status, Body: strings.TrimSpace(string(body))}
	}
	return json.Unmarshal(body, out)
}

func (c *Client) post(ctx context.Context, path string, v any) error {
	var body []byte
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		body = b
	}
	status, res, err := c.Do(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &Error{Status: status, Body: strings.TrimSpace(string(res))}
	}
	return nil
}

// ServerConfiguration is the raw Panel configuration payload. Both parts are
// kept as raw JSON so that nothing is lost when they are stored in the CR and
// served back to agents (Wings' typed structs do not round-trip).
type ServerConfiguration struct {
	Settings             json.RawMessage `json:"settings"`
	ProcessConfiguration json.RawMessage `json:"process_configuration"`
}

// GetServerConfiguration fetches settings and process configuration.
func (c *Client) GetServerConfiguration(ctx context.Context, uuid string) (*ServerConfiguration, error) {
	var out ServerConfiguration
	if err := c.getJSON(ctx, "/servers/"+uuid, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetInstallationScript fetches the egg install script.
func (c *Client) GetInstallationScript(ctx context.Context, uuid string) (*remote.InstallationScript, error) {
	var out remote.InstallationScript
	if err := c.getJSON(ctx, "/servers/"+uuid+"/install", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListServers returns every server of the node (all pages).
func (c *Client) ListServers(ctx context.Context) ([]remote.RawServerData, error) {
	var all []remote.RawServerData
	for page := 1; ; page++ {
		var r struct {
			Data []remote.RawServerData `json:"data"`
			Meta remote.Pagination      `json:"meta"`
		}
		if err := c.getJSON(ctx, "/servers", map[string]string{"page": strconv.Itoa(page), "per_page": "50"}, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Data...)
		if r.Meta.LastPage == 0 || uint(page) >= r.Meta.LastPage {
			return all, nil
		}
	}
}

// ResetServersState clears installing/restoring flags on the Panel.
func (c *Client) ResetServersState(ctx context.Context) error {
	return c.post(ctx, "/servers/reset", nil)
}

// PushServerStateChange forwards a container/status post.
func (c *Client) PushServerStateChange(ctx context.Context, uuid, prev, next string) error {
	return c.post(ctx, "/servers/"+uuid+"/container/status", map[string]any{"data": remote.ServerStateChange{PrevState: prev, NewState: next}})
}

// SetInstallationStatus forwards an install result.
func (c *Client) SetInstallationStatus(ctx context.Context, uuid string, successful, reinstall bool) error {
	return c.post(ctx, "/servers/"+uuid+"/install", remote.InstallStatusRequest{Successful: successful, Reinstall: reinstall})
}

// ValidateSftpCredentials asks the Panel to authenticate an SFTP login.
func (c *Client) ValidateSftpCredentials(ctx context.Context, req remote.SftpAuthRequest) (*remote.SftpAuthResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	status, body, err := c.Do(ctx, http.MethodPost, "/sftp/auth", b, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &Error{Status: status, Body: strings.TrimSpace(string(body))}
	}
	var out remote.SftpAuthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
