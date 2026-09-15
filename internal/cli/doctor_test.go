package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp/fake"
)

// instanceSpec is what an instance endpoint answers: no ADSModule, because
// listing instances is a controller's job.
func instanceSpec() map[string]any {
	return map[string]any{
		"Core": map[string]any{
			"Login":              map[string]any{},
			"GetStatus":          map[string]any{},
			"GetUpdates":         map[string]any{},
			"SendConsoleMessage": map[string]any{},
			"GetAPISpec":         map[string]any{},
		},
		"MinecraftModule": map[string]any{"GetHeadByUUID": map[string]any{}},
	}
}

func doctorFlags(t *testing.T, url string) *ampFlags {
	t.Helper()
	t.Setenv("AMPBB_AMP_PASSWORD", "s3cret")
	return &ampFlags{url: url, user: "backup", timeout: 5 * time.Second}
}

func findCheck(checks []check, name string) (check, bool) {
	for _, c := range checks {
		if c.name == name {
			return c, true
		}
	}
	return check{}, false
}

func failures(checks []check) []string {
	var out []string
	for _, c := range checks {
		if c.level == levelFail {
			out = append(out, c.name+": "+c.detail)
		}
	}
	return out
}

// An instance endpoint has no ADSModule. That is normal, and must not be
// reported as a broken AMP build -- the bug this test pins down.
func TestDoctorAcceptsInstanceEndpoint(t *testing.T) {
	srv := fake.New("backup", "s3cret")
	defer srv.Close()
	srv.Responder = func(module, method string, _ map[string]any) (any, bool) {
		switch module + "." + method {
		case "Core.GetAPISpec":
			return instanceSpec(), true
		case "ADSModule.GetLocalInstances":
			return map[string]any{"Title": "Invalid Module",
				"Message": "No such module loaded: 'ADSModule'"}, true
		}
		return nil, false
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	checks := ampChecks(context.Background(), doctorFlags(t, srv.URL), "SebsModpackv401", root)
	if f := failures(checks); len(f) > 0 {
		t.Fatalf("a correctly configured instance must pass, got failures: %v", f)
	}
	if c, ok := findCheck(checks, "endpoint"); !ok {
		t.Error("expected the report to name the endpoint kind")
	} else if !strings.Contains(c.detail, "not a controller") {
		t.Errorf("endpoint detail = %q, want it to say this is an instance", c.detail)
	}
	if c, ok := findCheck(checks, "instance SebsModpackv401"); !ok || c.detail != root {
		t.Errorf("expected the root %q to be reported as readable, got %+v", root, c)
	}
}

// Against a controller the instance directory is still looked up through the
// API, so that path must keep working.
func TestDoctorResolvesRootThroughController(t *testing.T) {
	srv := fake.New("backup", "s3cret")
	defer srv.Close()

	checks := ampChecks(context.Background(), doctorFlags(t, srv.URL), "SebsModpackv401", "")
	if c, ok := findCheck(checks, "instances"); !ok {
		t.Error("a controller should list its instances")
	} else if !strings.Contains(c.detail, "SebsModpackv401") {
		t.Errorf("instances = %q, want it to name SebsModpackv401", c.detail)
	}
	// The fake reports a BasePath that does not exist on this machine, which
	// is exactly what an unreadable instance directory looks like.
	c, ok := findCheck(checks, "instance SebsModpackv401")
	if !ok || c.level != levelFail {
		t.Errorf("an unreadable instance directory must fail, got %+v", c)
	}
}

// Without --root an instance endpoint cannot say where the instance lives.
// That is a warning with a remedy, not a failure.
func TestDoctorInstanceEndpointWithoutRoot(t *testing.T) {
	srv := fake.New("backup", "s3cret")
	defer srv.Close()
	srv.Responder = func(module, method string, _ map[string]any) (any, bool) {
		if module+"."+method == "Core.GetAPISpec" {
			return instanceSpec(), true
		}
		return nil, false
	}

	checks := ampChecks(context.Background(), doctorFlags(t, srv.URL), "SebsModpackv401", "")
	if f := failures(checks); len(f) > 0 {
		t.Fatalf("missing --root is not a failure, got: %v", f)
	}
	c, ok := findCheck(checks, "instance SebsModpackv401")
	if !ok || c.level != levelWarn || !strings.Contains(c.detail, "--root") {
		t.Errorf("expected a warning pointing at --root, got %+v", c)
	}
}

func TestRootDetail(t *testing.T) {
	dir := t.TempDir()
	if detail, lvl := rootDetail(dir); lvl != levelOK || detail != dir {
		t.Errorf("readable dir: got (%q, %v), want (%q, ok)", detail, lvl, dir)
	}

	missing := filepath.Join(dir, "nope")
	if _, lvl := rootDetail(missing); lvl != levelFail {
		t.Error("a missing directory must fail")
	}

	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if detail, lvl := rootDetail(file); lvl != levelFail || !strings.Contains(detail, "not a directory") {
		t.Errorf("a file is not an instance root: got (%q, %v)", detail, lvl)
	}
}
