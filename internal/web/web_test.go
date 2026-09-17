package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	url1 "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp/fake"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/auth"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/jobs"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/ops"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/state"
)

type rig struct {
	server   *httptest.Server
	amp      *fake.Server
	repo     *repo.Repository
	settings *settings.Store
	jobs     *jobs.Runner
	ops      *ops.Runner
	client   *http.Client
	csrf     string
}

func newRig(t *testing.T, spec map[string]map[string]any) *rig {
	t.Helper()

	root := t.TempDir()
	for rel, body := range map[string]string{
		"server.properties":               "level-name=survival_world\n",
		"survival_world/level.dat":        "world data\n",
		"survival_world/region/r.0.0.mca": "region data\n",
		"mods/[1.21.1] Awkward.jar":       "a mod\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r, err := repo.Init(t.TempDir(), 3)
	if err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	cfg := settings.Defaults()
	cfg.Instance = settings.Instance{Name: "Demo", Root: root}
	cfg.Schedule.Quiesce = false
	store, err := settings.NewMemory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	ampSrv := fake.New("backup", "pw")
	t.Cleanup(ampSrv.Close)
	ampSrv.AddSession("panel-session")
	if spec != nil {
		ampSrv.Spec = spec
	}

	service, err := amp.New(amp.Config{BaseURL: ampSrv.URL, Username: "backup", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	userClient := func(session string) (*amp.Client, error) {
		return amp.NewWithSession(amp.SessionConfig{BaseURL: ampSrv.URL, Session: session})
	}

	runner := jobs.NewRunner(jobs.Options{Throttle: time.Millisecond})
	operations := &ops.Runner{
		Repo: r, Settings: store, ToolVersion: "test", ScratchDir: t.TempDir(),
		Instance: func(context.Context) (*amp.Client, error) { return service, nil },
	}

	handler, err := NewHandler(Deps{
		Repo: r, Settings: store, State: stateStore, Jobs: runner, Ops: operations,
		Sessions:      auth.NewStore(0, 0, nil),
		Validator:     auth.AMPValidator{Dial: userClient},
		UserClient:    userClient,
		ServiceClient: func(context.Context) (*amp.Client, error) { return service, nil },
		Version:       "test",
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})

	jar := &cookieJar{}
	return &rig{
		server: srv, amp: ampSrv, repo: r, settings: store, jobs: runner, ops: operations,
		client: &http.Client{Jar: jar, Timeout: 10 * time.Second},
	}
}

// cookieJar is the smallest thing that satisfies http.CookieJar for one host.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url1.URL, cookies []*http.Cookie) { j.cookies = cookies }
func (j *cookieJar) Cookies(_ *url1.URL) []*http.Cookie             { return j.cookies }

func (r *rig) signIn(t *testing.T) {
	t.Helper()
	var out struct {
		CSRF string `json:"csrf"`
	}
	r.do(t, "POST", "/amp-bb/api/session",
		map[string]any{"session": "panel-session", "user": "gerry"}, http.StatusOK, &out)
	r.csrf = out.CSRF
}

func (r *rig) do(t *testing.T, method, path string, body any, wantStatus int, out any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, r.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.csrf != "" {
		req.Header.Set(csrfHeader, r.csrf)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decoding %s %s: %v (%s)", method, path, err, raw)
		}
	}
	return resp
}

func (r *rig) backup(t *testing.T) *repo.Manifest {
	t.Helper()
	m, err := r.ops.Backup(context.Background(), ops.Discard{}, ops.BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return m
}

// --- tests ------------------------------------------------------------------

func TestEverythingNeedsASession(t *testing.T) {
	rig := newRig(t, nil)
	for _, path := range []string{"/amp-bb/api/status", "/amp-bb/api/snapshots", "/amp-bb/api/settings", "/amp-bb/api/events"} {
		resp, err := rig.client.Get(rig.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestHealthzNeedsNothing(t *testing.T) {
	rig := newRig(t, nil)
	resp, err := rig.client.Get(rig.server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
}

func TestTheAssetsAreServedWithoutASession(t *testing.T) {
	rig := newRig(t, nil)
	// AMP's loader fetches Plugin.js before anybody has signed in here.
	for _, name := range []string{"Plugin.js", "tab.html", "amp-bb.css"} {
		resp, err := rig.client.Get(rig.server.URL + PluginPrefix + name)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", name, resp.StatusCode)
			continue
		}
		if len(raw) == 0 {
			t.Errorf("%s is empty", name)
		}
		if resp.Header.Get("ETag") == "" {
			t.Errorf("%s has no ETag", name)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s may be cached without revalidating; a stale Plugin.js is a bug that looks like a broken feature", name)
		}
	}
}

func TestSignInRejectsAnUnknownSession(t *testing.T) {
	rig := newRig(t, nil)
	rig.do(t, "POST", "/amp-bb/api/session",
		map[string]any{"session": "not-a-session"}, http.StatusUnauthorized, nil)
}

func TestSignInIssuesACookieAndACSRFToken(t *testing.T) {
	rig := newRig(t, nil)
	var out struct {
		CSRF         string                   `json:"csrf"`
		User         string                   `json:"user"`
		UserVerified bool                     `json:"user_verified"`
		Capabilities map[auth.Capability]bool `json:"capabilities"`
	}
	resp := rig.do(t, "POST", "/amp-bb/api/session",
		map[string]any{"session": "panel-session", "user": "gerry"}, http.StatusOK, &out)

	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no session cookie")
	}
	if !found.HttpOnly || found.SameSite != http.SameSiteStrictMode || found.Path != Prefix {
		t.Errorf("cookie = %+v; it must be HttpOnly, SameSite=Strict and scoped to the tab", found)
	}
	if out.CSRF == "" {
		t.Error("no CSRF token")
	}
	if out.User != "gerry" || out.UserVerified {
		t.Errorf("User = %q verified = %v; AMP cannot confirm a name", out.User, out.UserVerified)
	}
	if !out.Capabilities[auth.CapRestore] {
		t.Errorf("capabilities = %v", out.Capabilities)
	}
}

// SameSite=Strict already covers this; the explicit token is the belt.
func TestAWriteWithoutTheCSRFTokenIsRefused(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	saved := rig.csrf
	rig.csrf = ""
	rig.do(t, "POST", "/amp-bb/api/jobs/backup", map[string]any{}, http.StatusForbidden, nil)
	rig.csrf = "wrong-" + saved
	rig.do(t, "POST", "/amp-bb/api/jobs/backup", map[string]any{}, http.StatusForbidden, nil)
}

// A read-only account gets a read-only tab, and is told which permission it is
// missing rather than shown a button that does nothing.
func TestCapabilitiesGateTheEndpoints(t *testing.T) {
	rig := newRig(t, fake.BackupAccountSpec())
	rig.signIn(t)

	rig.do(t, "GET", "/amp-bb/api/status", nil, http.StatusOK, nil)
	rig.do(t, "POST", "/amp-bb/api/jobs/backup", map[string]any{}, http.StatusAccepted, nil)

	var body errorBody
	rig.do(t, "POST", "/amp-bb/api/jobs/restore",
		map[string]any{"snapshot": "x"}, http.StatusForbidden, &body)
	if !strings.Contains(body.Detail, "restore") {
		t.Errorf("detail = %q, want the missing permission named", body.Detail)
	}
	rig.do(t, "PUT", "/amp-bb/api/settings", settings.Defaults(), http.StatusForbidden, nil)
	rig.do(t, "POST", "/amp-bb/api/instance/stop", map[string]any{}, http.StatusForbidden, nil)
}

func TestStatusReportsTheThingsTheTabShows(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	rig.backup(t)

	var st Status
	rig.do(t, "GET", "/amp-bb/api/status", nil, http.StatusOK, &st)
	if st.Instance.Name != "Demo" {
		t.Errorf("instance = %q", st.Instance.Name)
	}
	if st.Instance.State != "ready" {
		t.Errorf("state = %q", st.Instance.State)
	}
	if st.Repo.Stats.Snapshots != 1 {
		t.Errorf("snapshots = %d", st.Repo.Stats.Snapshots)
	}
	if st.ServerNow.IsZero() {
		t.Error("no server time, so the browser cannot render relative times against our clock")
	}
	if st.Version != "test" {
		t.Errorf("version = %q", st.Version)
	}
}

// AMP going away must not take the status page with it: the snapshots are
// still there and still restorable.
func TestStatusSurvivesAMPBeingUnreachable(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	rig.amp.Close()

	var st Status
	rig.do(t, "GET", "/amp-bb/api/status", nil, http.StatusOK, &st)
	if st.Instance.Error == "" {
		t.Error("the failure was not reported")
	}
	if st.Instance.State != "unknown" {
		t.Errorf("state = %q, want unknown", st.Instance.State)
	}
}

func TestSnapshotsAreListedNewestFirst(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	first := rig.backup(t)
	if err := os.WriteFile(filepath.Join(rig.settings.Get().Instance.Root, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := rig.backup(t)

	var out struct {
		Snapshots []repo.Manifest `json:"snapshots"`
	}
	rig.do(t, "GET", "/amp-bb/api/snapshots", nil, http.StatusOK, &out)
	if len(out.Snapshots) != 2 {
		t.Fatalf("got %d snapshots", len(out.Snapshots))
	}
	if out.Snapshots[0].ID != second.ID || out.Snapshots[1].ID != first.ID {
		t.Errorf("order = %s, %s; want newest first", out.Snapshots[0].ID, out.Snapshots[1].ID)
	}
}

func TestTheTreeCanBeWalked(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	m := rig.backup(t)

	var root struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	rig.do(t, "GET", "/amp-bb/api/snapshots/"+m.ID+"/tree", nil, http.StatusOK, &root)
	if root.Total == 0 {
		t.Fatal("the root is empty")
	}

	var mods struct {
		Entries []map[string]any `json:"entries"`
	}
	rig.do(t, "GET", "/amp-bb/api/snapshots/"+m.ID+"/tree?path=mods", nil, http.StatusOK, &mods)
	var sawAwkward bool
	for _, e := range mods.Entries {
		if e["name"] == "[1.21.1] Awkward.jar" {
			sawAwkward = true
		}
	}
	if !sawAwkward {
		t.Errorf("mods = %v; the name with brackets did not survive", mods.Entries)
	}

	rig.do(t, "GET", "/amp-bb/api/snapshots/"+m.ID+"/tree?path=nowhere", nil, http.StatusNotFound, nil)
	rig.do(t, "GET", "/amp-bb/api/snapshots/nonsense/tree", nil, http.StatusNotFound, nil)
}

func TestAFileCanBeFetchedOutOfASnapshot(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	m := rig.backup(t)

	resp, err := rig.client.Get(rig.server.URL + "/amp-bb/api/snapshots/" + m.ID +
		"/file?path=survival_world%2Flevel.dat")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	if string(raw) != "world data\n" {
		t.Errorf("body = %q", raw)
	}
	// A file out of a backup is never rendered in the panel's own origin.
	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("Content-Disposition = %q", resp.Header.Get("Content-Disposition"))
	}
}

func TestRestoreIntoTheInstanceIsRefusedWhileItRuns(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	m := rig.backup(t)
	rig.amp.SetState(int(amp.StateReady))

	var body errorBody
	rig.do(t, "POST", "/amp-bb/api/jobs/restore", map[string]any{
		"snapshot": m.ID, "target": "instance",
	}, http.StatusConflict, &body)

	// The tab turns these two fields into the banner, so they are part of the
	// contract rather than decoration.
	if body.Reason != "instance_running" || body.State != "ready" {
		t.Errorf("body = %+v", body)
	}
}

func TestAScratchRestoreRunsWhileTheServerIsUp(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	m := rig.backup(t)
	rig.amp.SetState(int(amp.StateReady))

	var out struct {
		Job jobs.Job `json:"job"`
	}
	rig.do(t, "POST", "/amp-bb/api/jobs/restore", map[string]any{
		"snapshot": m.ID, "target": "scratch", "paths": []string{"survival_world/level.dat"},
	}, http.StatusAccepted, &out)

	job := waitForJob(t, rig, out.Job.ID)
	if job.State != jobs.StateSucceeded {
		t.Fatalf("job = %s: %s", job.State, job.Error)
	}
	var result ops.RestoreResult
	if err := json.Unmarshal(job.Result, &result); err != nil {
		t.Fatalf("result: %v", err)
	}
	if result.Report.Files != 1 {
		t.Errorf("restored %d files, want 1", result.Report.Files)
	}
}

// The browser sends a minimal cover, so reaching the cap means something is
// wrong on the other side. An honest limit beats an unbounded request body.
func TestTooManySelectedPathsAreRefused(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	paths := make([]string, maxRestorePaths+1)
	for i := range paths {
		paths[i] = "a/b/c"
	}
	rig.do(t, "POST", "/amp-bb/api/jobs/restore", map[string]any{
		"snapshot": "x", "paths": paths,
	}, http.StatusRequestEntityTooLarge, nil)
}

func TestASecondJobIsTurnedAwayWithTheFirst(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)

	release := make(chan struct{})
	defer close(release)
	blocking, err := rig.jobs.Submit(jobs.KindRestore, "someone", func(*jobs.Handle) (any, error) {
		<-release
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var body errorBody
	rig.do(t, "POST", "/amp-bb/api/jobs/backup", map[string]any{}, http.StatusConflict, &body)
	if body.Reason != "busy" || body.Job == nil || body.Job.ID != blocking.ID {
		t.Errorf("body = %+v; the tab needs the running job to jump to it", body)
	}
}

func TestSettingsRoundTripAndRejectNonsense(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)

	var loaded struct {
		Settings settings.File `json:"settings"`
		Defaults []string      `json:"default_excludes"`
	}
	rig.do(t, "GET", "/amp-bb/api/settings", nil, http.StatusOK, &loaded)
	if loaded.Settings.Schedule.Every != "1h" {
		t.Errorf("every = %q", loaded.Settings.Schedule.Every)
	}
	if len(loaded.Defaults) == 0 {
		t.Error("the built-in exclusions were not sent, so the editor cannot show them")
	}

	next := loaded.Settings
	next.Schedule.Every = "15m"
	var saved struct {
		Settings settings.File `json:"settings"`
	}
	rig.do(t, "PUT", "/amp-bb/api/settings", next, http.StatusOK, &saved)
	if saved.Settings.Schedule.Every != "15m" {
		t.Errorf("every = %q after saving", saved.Settings.Schedule.Every)
	}
	if saved.Settings.UpdatedBy == "" {
		t.Error("no author was recorded for a change that decides which snapshots get deleted")
	}

	bad := saved.Settings
	bad.Schedule.Every = "sometimes"
	rig.do(t, "PUT", "/amp-bb/api/settings", bad, http.StatusBadRequest, nil)
	if rig.settings.Get().Schedule.Every != "15m" {
		t.Error("a rejected change leaked into the running settings")
	}
}

// The instance root is not editable over HTTP: an absolute path in a request
// body is a write primitive.
func TestSettingsCannotRedirectTheBackup(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	next := rig.settings.Get()
	next.Instance.Root = "/etc"
	var saved struct {
		Settings settings.File `json:"settings"`
	}
	rig.do(t, "PUT", "/amp-bb/api/settings", next, http.StatusOK, &saved)
	if saved.Settings.Instance.Root == "/etc" {
		t.Fatal("the instance root was editable through the API")
	}
}

func TestExclusionPreviewCountsAndRefusesBadPatterns(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	rig.backup(t)

	var preview ops.ExclusionPreview
	rig.do(t, "POST", "/amp-bb/api/settings/validate",
		map[string]any{"patterns": []string{"survival_world"}, "use_defaults": false},
		http.StatusOK, &preview)
	if preview.Excluded == 0 {
		t.Error("the preview says nothing would be left out")
	}

	rig.do(t, "POST", "/amp-bb/api/settings/validate",
		map[string]any{"patterns": []string{"mods/[unclosed"}}, http.StatusBadRequest, nil)
}

// Stopping runs as the caller, not as the service account, which deliberately
// cannot do it.
func TestStoppingRunsAsTheCaller(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	rig.amp.SetState(int(amp.StateReady))

	var out struct {
		Job jobs.Job `json:"job"`
	}
	rig.do(t, "POST", "/amp-bb/api/instance/stop", map[string]any{}, http.StatusAccepted, &out)

	// The fake moves to "stopping"; settle it the way a real server would.
	go func() {
		time.Sleep(20 * time.Millisecond)
		rig.amp.SetState(int(amp.StateStopped))
	}()
	job := waitForJob(t, rig, out.Job.ID)
	if job.State != jobs.StateSucceeded {
		t.Fatalf("job = %s: %s", job.State, job.Error)
	}
	if rig.amp.CommandLog() != nil && len(rig.amp.CommandLog()) > 0 {
		t.Errorf("the console was used to stop the server: %v", rig.amp.CommandLog())
	}
}

// A session whose AMP permissions do not include stopping must not be able to.
func TestStoppingIsRefusedWithoutThePermission(t *testing.T) {
	rig := newRig(t, fake.BackupAccountSpec())
	rig.signIn(t)
	rig.do(t, "POST", "/amp-bb/api/instance/stop", map[string]any{}, http.StatusForbidden, nil)
}

func TestTheEventStreamReplaysAndHeartbeats(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)

	// Produce something to replay.
	job, err := rig.jobs.Submit(jobs.KindCheck, "gerry", func(h *jobs.Handle) (any, error) {
		h.Logf("info", "a line worth replaying")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, rig, job.ID)

	req, err := http.NewRequest("GET", rig.server.URL+"/amp-bb/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rig.client.Jar.Cookies(nil) {
		req.AddCookie(c)
	}
	resp, err := rig.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	// Without this nginx buffers the stream and it appears dead.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	var sawLog bool
	for time.Now().Before(deadline) && !sawLog {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "a line worth replaying") {
			sawLog = true
		}
	}
	if !sawLog {
		t.Error("the backlog was not replayed to a new subscriber")
	}
}

func TestForgetAndUnforget(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	m := rig.backup(t)

	rig.do(t, "POST", "/amp-bb/api/snapshots/"+m.ID+"/forget", map[string]any{}, http.StatusOK, nil)
	var out struct {
		Snapshots []repo.Manifest `json:"snapshots"`
	}
	rig.do(t, "GET", "/amp-bb/api/snapshots", nil, http.StatusOK, &out)
	if len(out.Snapshots) != 0 {
		t.Errorf("the forgotten snapshot is still listed")
	}

	rig.do(t, "POST", "/amp-bb/api/snapshots/"+m.ID+"/unforget", map[string]any{}, http.StatusOK, nil)
	rig.do(t, "GET", "/amp-bb/api/snapshots", nil, http.StatusOK, &out)
	if len(out.Snapshots) != 1 {
		t.Errorf("unforget did not bring it back")
	}
}

func TestSignOutEndsTheSession(t *testing.T) {
	rig := newRig(t, nil)
	rig.signIn(t)
	rig.do(t, "DELETE", "/amp-bb/api/session", nil, http.StatusNoContent, nil)
	rig.do(t, "GET", "/amp-bb/api/status", nil, http.StatusUnauthorized, nil)
}

func waitForJob(t *testing.T, rig *rig, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, _, err := rig.jobs.Get(id)
		if err == nil && job.State.Done() {
			return job
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
	return jobs.Job{}
}
