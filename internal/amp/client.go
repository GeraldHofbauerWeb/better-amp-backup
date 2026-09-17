// Package amp speaks CubeCoders AMP's JSON API.
//
// The API is uniform: every call is a POST to /API/<Module>/<Method> with the
// parameters as a JSON object, authenticated by a SESSIONID *field in that
// object* obtained from Core.Login. It is not a header: AMP ignores a SESSIONID
// header entirely, and an ignored session is not an error but an anonymous
// call, which fails later and somewhere else -- Core.GetAPISpec quietly shrinks
// to the handful of methods an unauthenticated caller may see, and everything
// else answers "not authorised". Measured against AMP 2.8.0.4.
//
// There are no API keys, so the client holds credentials and re-authenticates
// when a session expires. A client built with NewWithSession is the exception:
// it borrows a session obtained elsewhere -- the panel session of whoever is
// logged in -- and can therefore do exactly what that person can do and no
// more. When that session goes, so does the client.
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

// SessionConfig describes a client that borrows a session obtained elsewhere,
// in practice the panel session of whoever is logged in.
//
// It has no credentials, so a rejected session is returned as ErrUnauthorized
// rather than retried: there is nothing to retry with. That is the point --
// such a client can do exactly what that person could do, and nothing more.
type SessionConfig struct {
	BaseURL            string
	Session            string
	Timeout            time.Duration
	InsecureSkipVerify bool
}

// sessionState is the mutable half of a client. It is held by pointer so that
// ForInstance can clone a client without copying a mutex -- and so that a
// re-login on either client is seen by both.
type sessionState struct {
	mu      sync.Mutex
	session string
}

// Client is an AMP API client. It is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
	// route is inserted between /API and the module for calls that go through
	// the controller's instance proxy. Empty means this client addresses its
	// endpoint directly.
	route string
	state *sessionState
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
		cfg:   cfg,
		http:  &http.Client{Timeout: cfg.Timeout, Transport: transport},
		state: &sessionState{},
	}, nil
}

// NewWithSession builds a client that uses a session someone else obtained.
func NewWithSession(cfg SessionConfig) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("amp: no base URL")
	}
	if cfg.Session == "" {
		return nil, errors.New("amp: no session")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	transport := http.DefaultTransport
	if cfg.InsecureSkipVerify {
		transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return &Client{
		cfg:   Config{BaseURL: strings.TrimRight(cfg.BaseURL, "/"), Timeout: cfg.Timeout},
		http:  &http.Client{Timeout: cfg.Timeout, Transport: transport},
		state: &sessionState{session: cfg.Session},
	}, nil
}

// ForInstance returns a client that routes every call through the controller's
// instance proxy -- /API/ADSModule/Servers/<id>/API/<Module>/<Method> -- rather
// than addressing a module on this endpoint directly.
//
// This is the path AMP's own panel uses once you open an instance: the URL is
// the controller's, and the session is one issued for that instance. The
// returned client shares this one's session, so re-authenticating on either is
// visible to both.
func (c *Client) ForInstance(instanceID string) *Client {
	clone := *c
	clone.route = "/ADSModule/Servers/" + instanceID + "/API"
	return &clone
}

// Session returns the session this client is using, logging in first if it has
// credentials and no session yet.
func (c *Client) Session(ctx context.Context) (string, error) {
	return c.sessionOrLogin(ctx)
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

	c.state.mu.Lock()
	c.state.session = res.SessionID
	c.state.mu.Unlock()
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
	if c.cfg.Username == "" {
		// A borrowed session cannot be renewed, and AMP answers "Unauthorized
		// Access" both for a session it has forgotten and for a method this
		// account may not call. Discarding the session on the second of those
		// would throw away a perfectly good one over a permission the caller
		// never had -- and every later call would fail for the wrong reason.
		return err
	}

	// The session expired between calls. Drop it and try exactly once more, so
	// a genuinely wrong password cannot turn into a retry loop.
	c.state.mu.Lock()
	if c.state.session == session {
		c.state.session = ""
	}
	c.state.mu.Unlock()

	session, err = c.sessionOrLogin(ctx)
	if err != nil {
		return err
	}
	return c.post(ctx, module, method, params, session, out)
}

func (c *Client) sessionOrLogin(ctx context.Context) (string, error) {
	c.state.mu.Lock()
	s := c.state.session
	c.state.mu.Unlock()
	if s != "" {
		return s, nil
	}
	if c.cfg.Username == "" {
		// A borrowed session that AMP no longer accepts cannot be renewed
		// here. Saying so is the whole contract of NewWithSession.
		return "", ErrUnauthorized
	}
	if err := c.Login(ctx); err != nil {
		return "", err
	}
	c.state.mu.Lock()
	s = c.state.session
	c.state.mu.Unlock()
	return s, nil
}

func (c *Client) post(ctx context.Context, module, method string, params map[string]any,
	session string, out any) error {

	// Copy rather than mutate: params belongs to the caller, and the session
	// must not leak into a map it might reuse or log.
	body := make(map[string]any, len(params)+1)
	for k, v := range params {
		body[k] = v
	}
	if session != "" {
		body["SESSIONID"] = session
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("amp: encode %s.%s: %w", module, method, err)
	}

	url := fmt.Sprintf("%s/API%s/%s/%s", c.cfg.BaseURL, c.route, module, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("amp: build request for %s.%s: %w", module, method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

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
