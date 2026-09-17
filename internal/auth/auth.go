// Package auth trades a caller's AMP panel session for a session of our own.
//
// amp-bb has no user accounts and will not grow any. A backup tool that grows
// its own user database is how a backup tool becomes a breach. Identity is
// AMP's, and so is authorisation: what a session may do here is derived from
// what AMP's own permission-filtered API spec says it may do there.
//
// The exchange happens once, because validating against AMP costs a round trip
// and the browser makes a request per click. What comes back is a cookie.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
)

// ErrRejected is returned for a session AMP does not recognise. It carries no
// detail: telling an anonymous caller why their guess failed is free help.
var ErrRejected = errors.New("auth: session rejected")

// Capability is one thing a session is allowed to do.
type Capability string

const (
	CapRead     Capability = "read"     // snapshots, the file tree, status, settings
	CapBackup   Capability = "backup"   // take one now
	CapRestore  Capability = "restore"  // put one back
	CapSettings Capability = "settings" // schedule, retention, exclusions
	CapDestroy  Capability = "destroy"  // forget, prune
	CapStop     Capability = "stop"
	CapStart    Capability = "start"
)

// Session is one authenticated caller.
type Session struct {
	ID   string `json:"-"`
	CSRF string `json:"-"`
	// AMP is the caller's panel session. It is used to act as them -- stopping
	// and starting the instance -- and never written to a log.
	AMP string `json:"-"`

	// User is who this is, as far as we can tell. AMP 2.8 has no call that
	// names the session's own user: GetAMPUserInfo wants a username, and
	// GetActiveAMPSessions does not report session ids, so there is nothing to
	// match on. When the panel tells us a name we keep it and say plainly that
	// it was not verified, rather than pretending to an audit trail we do not
	// have.
	User         string `json:"user"`
	UserVerified bool   `json:"user_verified"`

	Caps    map[Capability]bool `json:"capabilities"`
	Control amp.Control         `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// Can reports whether the session holds a capability.
func (s *Session) Can(c Capability) bool { return s != nil && s.Caps[c] }

// Identity renders who did something, for a record that will be read later.
func (s *Session) Identity() string {
	if s == nil || s.User == "" {
		return "unknown"
	}
	if s.UserVerified {
		return s.User
	}
	return s.User + " (unverified)"
}

// Validator turns a panel session into an identity and a capability set.
type Validator interface {
	Validate(ctx context.Context, ampSession string) (Identity, error)
}

// Identity is what a Validator establishes.
type Identity struct {
	User     string
	Verified bool
	Caps     map[Capability]bool
	Control  amp.Control
}

// Store holds live sessions.
type Store struct {
	idle  time.Duration
	max   time.Duration
	clock func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewStore builds a store. idle is how long a session survives without use;
// max is how long it survives at all.
func NewStore(idle, max time.Duration, clock func() time.Time) *Store {
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	if max <= 0 {
		max = 12 * time.Hour
	}
	if clock == nil {
		clock = time.Now
	}
	return &Store{idle: idle, max: max, clock: clock, sessions: map[string]*Session{}}
}

// Exchange validates a panel session and issues one of ours.
//
// claimedUser is what the panel says the person is called. It is not trusted
// for any decision; see Session.User.
func (s *Store) Exchange(ctx context.Context, ampSession, claimedUser string, v Validator) (*Session, error) {
	if strings.TrimSpace(ampSession) == "" {
		return nil, ErrRejected
	}
	id, err := v.Validate(ctx, ampSession)
	if err != nil {
		return nil, err
	}
	if len(id.Caps) == 0 {
		return nil, ErrRejected
	}

	user, verified := id.User, id.Verified
	if user == "" && claimedUser != "" {
		user, verified = sanitiseName(claimedUser), false
	}

	now := s.clock()
	sess := &Session{
		ID:           newToken(),
		CSRF:         newToken(),
		AMP:          ampSession,
		User:         user,
		UserVerified: verified,
		Caps:         id.Caps,
		Control:      id.Control,
		CreatedAt:    now,
		LastSeen:     now,
	}

	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	return sess, nil
}

// Lookup returns a live session and marks it used.
func (s *Store) Lookup(id string) (*Session, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	now := s.clock()
	if s.expiredLocked(sess, now) {
		delete(s.sessions, id)
		return nil, false
	}
	sess.LastSeen = now
	return sess, true
}

// Revoke drops a session, which is what signing out does.
func (s *Store) Revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// Sweep removes expired sessions and reports how many went.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	var removed int
	for id, sess := range s.sessions {
		if s.expiredLocked(sess, now) {
			delete(s.sessions, id)
			removed++
		}
	}
	return removed
}

// Len reports how many sessions are held, expired ones included.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Store) expiredLocked(sess *Session, now time.Time) bool {
	return now.Sub(sess.LastSeen) > s.idle || now.Sub(sess.CreatedAt) > s.max
}

// CheckCSRF compares a token in constant time.
func (s *Session) CheckCSRF(token string) bool {
	if s == nil || s.CSRF == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.CSRF), []byte(token)) == 1
}

// DeriveCapabilities turns a permission-filtered API spec into what a session
// may do here.
//
// The rule, stated plainly because it is a security decision: a caller who can
// stop and start the instance can already destroy its state by other means, so
// gating the destructive amp-bb operations on the same bar adds no exposure
// that was not already there. Anyone who can only read the console gets a
// read-only view and a "back up now" button, which cannot lose data.
func DeriveCapabilities(spec map[string]map[string]any) (map[Capability]bool, amp.Control) {
	ctl := amp.ControlMethods(spec)
	caps := map[Capability]bool{}

	// A spec this small is the anonymous surface AMP hands an unauthenticated
	// caller. It is not a session with few permissions; it is not a session.
	if !amp.HasMethod(spec, "Core", "GetStatus") {
		return caps, ctl
	}

	caps[CapRead] = true
	caps[CapBackup] = true
	if ctl.CanStop() {
		caps[CapStop] = true
	}
	if ctl.CanStart() {
		caps[CapStart] = true
	}
	if ctl.CanStop() && ctl.CanStart() {
		caps[CapRestore] = true
		caps[CapSettings] = true
		caps[CapDestroy] = true
	}
	return caps, ctl
}

// AMPValidator validates against a panel.
type AMPValidator struct {
	// Dial builds a client for a borrowed session, already pointed at the
	// instance: identity and permissions are the instance's, not the
	// controller's, and a controller session is not valid on an instance.
	Dial func(session string) (*amp.Client, error)
}

// Validate asks AMP what this session may do.
func (v AMPValidator) Validate(ctx context.Context, ampSession string) (Identity, error) {
	client, err := v.Dial(ampSession)
	if err != nil {
		return Identity{}, err
	}
	spec, err := client.GetAPISpec(ctx)
	if err != nil {
		if errors.Is(err, amp.ErrUnauthorized) {
			return Identity{}, ErrRejected
		}
		return Identity{}, fmt.Errorf("auth: asking AMP about the session: %w", err)
	}
	caps, ctl := DeriveCapabilities(spec)
	if len(caps) == 0 {
		return Identity{}, ErrRejected
	}
	return Identity{Caps: caps, Control: ctl}, nil
}

// sanitiseName keeps a client-supplied display name from carrying anything
// into a log line or a settings file that a reader would have to decode.
func sanitiseName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		name = name[:64]
	}
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	return cleaned
}

func newToken() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a token
		// that is not random is worse than no daemon.
		panic("auth: no randomness available: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
