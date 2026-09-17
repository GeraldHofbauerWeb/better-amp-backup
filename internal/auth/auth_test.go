package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp/fake"
)

func ampValidator(t *testing.T, srv *fake.Server) Validator {
	t.Helper()
	return AMPValidator{Dial: func(session string) (*amp.Client, error) {
		return amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: session})
	}}
}

// The permission-filtered spec is the capability list. An admin gets
// everything; the deliberately narrow service account gets a read-only view.
func TestCapabilitiesFollowTheSpec(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]map[string]any
		want map[Capability]bool
	}{
		{
			name: "an administrator",
			spec: fake.DefaultSpec(),
			want: map[Capability]bool{
				CapRead: true, CapBackup: true, CapStop: true, CapStart: true,
				CapRestore: true, CapSettings: true, CapDestroy: true,
			},
		},
		{
			name: "the backup service account",
			spec: fake.BackupAccountSpec(),
			want: map[Capability]bool{CapRead: true, CapBackup: true},
		},
		{
			name: "an anonymous caller",
			spec: map[string]map[string]any{"Core": {"Login": map[string]any{}}},
			want: map[Capability]bool{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := DeriveCapabilities(c.spec)
			if len(got) != len(c.want) {
				t.Fatalf("capabilities = %v, want %v", got, c.want)
			}
			for cap := range c.want {
				if !got[cap] {
					t.Errorf("missing %s", cap)
				}
			}
		})
	}
}

// Someone who may stop but not start is not who the restore bar was drawn for.
func TestRestoreNeedsBothHalvesOfTheBar(t *testing.T) {
	spec := fake.DefaultSpec()
	delete(spec["Core"], "Start")
	caps, ctl := DeriveCapabilities(spec)
	if caps[CapRestore] || caps[CapSettings] || caps[CapDestroy] {
		t.Errorf("caps = %v, want no destructive ones without Start", caps)
	}
	if !caps[CapStop] || ctl.CanStart() {
		t.Errorf("caps = %v, ctl = %+v", caps, ctl)
	}
}

func TestExchangeIssuesASessionForAValidOne(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")

	store := NewStore(0, 0, nil)
	sess, err := store.Exchange(context.Background(), "panel-session", "gerry", ampValidator(t, srv))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if sess.ID == "" || sess.CSRF == "" || sess.ID == sess.CSRF {
		t.Error("the session and CSRF tokens must both exist and differ")
	}
	if !sess.Can(CapRestore) {
		t.Errorf("caps = %v", sess.Caps)
	}
	if sess.User != "gerry" || sess.UserVerified {
		t.Errorf("User = %q verified = %v; AMP cannot confirm a name, so it must not claim to",
			sess.User, sess.UserVerified)
	}
	if got := sess.Identity(); !strings.Contains(got, "unverified") {
		t.Errorf("Identity() = %q, want it to admit what it does not know", got)
	}

	back, ok := store.Lookup(sess.ID)
	if !ok || back.ID != sess.ID {
		t.Error("the session could not be looked up again")
	}
}

func TestExchangeRejectsWhatAMPRejects(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()

	store := NewStore(0, 0, nil)
	_, err := store.Exchange(context.Background(), "never-issued", "gerry", ampValidator(t, srv))
	if !errors.Is(err, ErrRejected) {
		t.Errorf("err = %v, want ErrRejected", err)
	}
	if store.Len() != 0 {
		t.Error("a rejected exchange left a session behind")
	}

	if _, err := store.Exchange(context.Background(), "   ", "gerry", ampValidator(t, srv)); !errors.Is(err, ErrRejected) {
		t.Errorf("an empty session gave %v", err)
	}
}

// A rejection must not say why. Telling an anonymous caller whether a session
// was unknown or merely unprivileged is free help.
func TestRejectionSaysNothingUseful(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	store := NewStore(0, 0, nil)

	_, err := store.Exchange(context.Background(), "guess", "", ampValidator(t, srv))
	if err == nil {
		t.Fatal("accepted")
	}
	if strings.Contains(err.Error(), "guess") {
		t.Errorf("the error echoes the token back: %v", err)
	}
}

func TestSessionsExpire(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")

	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := NewStore(30*time.Minute, 12*time.Hour, clock)

	sess, err := store.Exchange(context.Background(), "panel-session", "gerry", ampValidator(t, srv))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// Used within the idle window, it survives and the window moves.
	now = now.Add(25 * time.Minute)
	if _, ok := store.Lookup(sess.ID); !ok {
		t.Fatal("the session expired inside its idle window")
	}
	now = now.Add(25 * time.Minute)
	if _, ok := store.Lookup(sess.ID); !ok {
		t.Fatal("using the session did not refresh it")
	}

	// Left alone for longer than the idle window, it goes.
	now = now.Add(31 * time.Minute)
	if _, ok := store.Lookup(sess.ID); ok {
		t.Error("an idle session survived")
	}
}

// The absolute cap exists so that a session cannot be kept alive for ever by
// a page that polls.
func TestSessionsExpireEvenWhenUsedConstantly(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")

	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	store := NewStore(30*time.Minute, 2*time.Hour, func() time.Time { return now })
	sess, _ := store.Exchange(context.Background(), "panel-session", "", ampValidator(t, srv))

	// Polled every ten minutes, so the idle window never lapses.
	for range 11 {
		now = now.Add(10 * time.Minute)
		if _, ok := store.Lookup(sess.ID); !ok {
			t.Fatalf("the session went early, at %s", now.Sub(sess.CreatedAt))
		}
	}
	// Past two hours, the cap has to win however diligently it was polled.
	now = now.Add(20 * time.Minute)
	if _, ok := store.Lookup(sess.ID); ok {
		t.Error("a session outlived its absolute cap by being polled")
	}
}

func TestSweepRemovesExpiredSessions(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")

	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	store := NewStore(time.Minute, time.Hour, func() time.Time { return now })
	for range 3 {
		if _, err := store.Exchange(context.Background(), "panel-session", "", ampValidator(t, srv)); err != nil {
			t.Fatalf("Exchange: %v", err)
		}
	}
	if store.Len() != 3 {
		t.Fatalf("Len = %d", store.Len())
	}
	now = now.Add(2 * time.Minute)
	if removed := store.Sweep(); removed != 3 {
		t.Errorf("Sweep removed %d, want 3", removed)
	}
	if store.Len() != 0 {
		t.Errorf("Len = %d after sweeping", store.Len())
	}
}

func TestRevokeEndsASession(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")
	store := NewStore(0, 0, nil)

	sess, _ := store.Exchange(context.Background(), "panel-session", "", ampValidator(t, srv))
	store.Revoke(sess.ID)
	if _, ok := store.Lookup(sess.ID); ok {
		t.Error("a revoked session still works")
	}
}

func TestCSRFComparison(t *testing.T) {
	sess := &Session{CSRF: "the-token"}
	if !sess.CheckCSRF("the-token") {
		t.Error("the right token was rejected")
	}
	for _, wrong := range []string{"", "the-toke", "the-tokenn", "THE-TOKEN"} {
		if sess.CheckCSRF(wrong) {
			t.Errorf("%q was accepted", wrong)
		}
	}
	var nilSession *Session
	if nilSession.CheckCSRF("anything") {
		t.Error("a nil session accepted a token")
	}
}

// A display name from a browser must not carry anything into a log line that a
// reader would have to decode.
func TestAClaimedNameIsCleanedUp(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")
	store := NewStore(0, 0, nil)

	sess, err := store.Exchange(context.Background(),
		"panel-session", "ger\nry\x00 <script>"+strings.Repeat("x", 200), ampValidator(t, srv))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if strings.ContainsAny(sess.User, "\n\x00") {
		t.Errorf("User = %q still holds control characters", sess.User)
	}
	if len(sess.User) > 64 {
		t.Errorf("User is %d characters long", len(sess.User))
	}
}

// Tokens have to be unguessable, and no two may collide.
func TestTokensAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		token := newToken()
		if len(token) < 40 {
			t.Fatalf("token %q is too short to be a secret", token)
		}
		if seen[token] {
			t.Fatal("two tokens collided")
		}
		seen[token] = true
	}
}

func TestConcurrentExchangeAndLookup(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")
	store := NewStore(0, 0, nil)
	v := ampValidator(t, srv)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				sess, err := store.Exchange(context.Background(), "panel-session", "gerry", v)
				if err != nil {
					t.Errorf("Exchange: %v", err)
					return
				}
				store.Lookup(sess.ID)
				store.Sweep()
				store.Revoke(sess.ID)
			}
		}()
	}
	wg.Wait()
}
