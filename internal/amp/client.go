// Package amp speaks CubeCoders AMP's JSON API.
//
// The API is uniform: every call is a POST to /API/<Module>/<Method> with the
// parameters as a JSON object, authenticated by a SESSIONID header obtained
// from Core.Login. There are no API keys, so the client holds credentials and
// re-authenticates when a session expires.
package amp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrUnauthorized indicates the session was rejected. Callers do not normally
// see it: the client re-authenticates once and retries.
var ErrUnauthorized = errors.New("amp: not authorised")

// Config describes how to reach an AMP instance.
type Config struct {
	// BaseURL is the instance's web root, e.g. http://127.0.0.1:8082.
	BaseURL  string
	Username string
	Password string
	// TwoFactorToken is the current TOTP code, when the account requires one.
	TwoFactorToken string
	// Timeout bounds a single request. Console calls are fast; the default is
	// generous enough for a busy panel without hanging a backup.
	Timeout time.Duration
	// InsecureSkipVerify disables TLS verification, for panels behind a
	// self-signed certificate. Off by default, deliberately.
	InsecureSkipVerify bool
}

// Client is an AMP API client. It is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client

	mu      sync.Mutex
	session string
}

// New builds a client. It does not contact the server; call Login or simply
// make a request, which authenticates on demand.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("amp: no base URL")
	}
	if cfg.Username == "" {
		return nil, errors.New("amp: no username")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	transport := http.DefaultTransport
	if cfg.InsecureSkipVerify {
		transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout, Transport: transport},
	}, nil
}

// loginResponse is what Core.Login returns. AMP reports failure by omitting
// sessionID rather than by HTTP status, so success is judged on the field.
type loginResponse struct {
	SessionID       string `json:"sessionID"`
	Success         *bool  `json:"success"`
	ResultReason    string `json:"resultReason"`
	TwoFactorNeeded bool   `json:"twoFactorRequired"`
}

// Login authenticates and stores the session.
func (c *Client) Login(ctx context.Context) error {
	body := map[string]any{
		"username":   c.cfg.Username,
		"password":   c.cfg.Password,
		"token":      c.cfg.TwoFactorToken,
		"rememberMe": false,
	}
	var res loginResponse
	if err := c.post(ctx, "Core", "Login", body, "", &res); err != nil {
		return fmt.Errorf("amp: login: %w", err)
	}
	if res.TwoFactorNeeded {
		return errors.New("amp: login rejected: the account requires a two-factor code")
	}
	if res.SessionID == "" {
		reason := res.ResultReason
		if reason == "" {
			reason = "no session returned"
		}
		// Never include the password or the body in this message.
		return fmt.Errorf("amp: login rejected for user %q: %s", c.cfg.Username, reason)
	}

	c.mu.Lock()
	c.session = res.SessionID
	c.mu.Unlock()
	return nil
}

// Call invokes an API method, authenticating first if necessary and retrying
// once if the session turned out to be stale.
//
// out may be nil when the response is not needed.
func (c *Client) Call(ctx context.Context, module, method string, params map[string]any, out any) error {
	session, err := c.sessionOrLogin(ctx)
	if err != nil {
		return err
	}

	err = c.post(ctx, module, method, params, session, out)
	if !errors.Is(err, ErrUnauthorized) {
		return err
	}

	// The session expired between calls. Drop it and try exactly once more, so
	// a genuinely wrong password cannot turn into a retry loop.
	c.mu.Lock()
	if c.session == session {
		c.session = ""
	}
	c.mu.Unlock()

	session, err = c.sessionOrLogin(ctx)
	if err != nil {
		return err
	}
	return c.post(ctx, module, method, params, session, out)
}

func (c *Client) sessionOrLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s != "" {
		return s, nil
	}
	if err := c.Login(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	s = c.session
	c.mu.Unlock()
	return s, nil
}

func (c *Client) post(ctx context.Context, module, method string, params map[string]any,
	session string, out any) error {

	if params == nil {
		params = map[string]any{}
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("amp: encode %s.%s: %w", module, method, err)
	}

	url := fmt.Sprintf("%s/API/%s/%s", c.cfg.BaseURL, module, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("amp: build request for %s.%s: %w", module, method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if session != "" {
		req.Header.Set("SESSIONID", session)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("amp: %s.%s: %w", module, method, err)
	}
	defer resp.Body.Close()

	// Cap the read: a misconfigured URL pointing at something that is not AMP
	// should not be able to exhaust memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("amp: read %s.%s response: %w", module, method, err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("amp: %s.%s returned HTTP %d: %s",
			module, method, resp.StatusCode, snippet(raw))
	}

	// AMP answers an expired session with HTTP 200 and an error object, so the
	// body has to be inspected as well.
	if isUnauthorizedBody(raw) {
		return ErrUnauthorized
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("amp: decode %s.%s response: %w (body: %s)",
			module, method, err, snippet(raw))
	}
	return nil
}

// isUnauthorizedBody recognises AMP's in-band session errors.
func isUnauthorizedBody(raw []byte) bool {
	var probe struct {
		Title  string `json:"Title"`
		Status any    `json:"Status"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	t := strings.ToLower(probe.Title)
	return strings.Contains(t, "unauthorized") ||
		strings.Contains(t, "unauthorised") ||
		strings.Contains(t, "session") && strings.Contains(t, "expired")
}

func snippet(raw []byte) string {
	const max = 300
	s := strings.TrimSpace(string(raw))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
