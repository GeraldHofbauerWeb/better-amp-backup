package amp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AMP authenticates a call by a SESSIONID field in the request body. Sending it
// as a header instead is not refused -- the call is simply treated as anonymous
// and fails much later, so this is pinned down here rather than in production.
func TestSessionTravelsInTheBody(t *testing.T) {
	var bodies []map[string]any
	var headerSeen bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("SESSIONID") != "" {
			headerSeen = true
		}
		var params map[string]any
		_ = json.NewDecoder(r.Body).Decode(&params)
		bodies = append(bodies, params)

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/API/Core/Login" {
			_, _ = w.Write([]byte(`{"sessionID":"sess-1","success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"State":20,"Uptime":"01:00:00"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Username: "backup", Password: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetStatus(context.Background()); err != nil {
		t.Fatalf("GetStatus: %v", err)
	}

	if headerSeen {
		t.Error("SESSIONID was sent as a header; AMP ignores it and the call becomes anonymous")
	}
	if len(bodies) != 2 {
		t.Fatalf("expected a login followed by the call, got %d requests", len(bodies))
	}
	if _, ok := bodies[0]["SESSIONID"]; ok {
		t.Error("the login itself must not carry a session")
	}
	if got := bodies[1]["SESSIONID"]; got != "sess-1" {
		t.Errorf("SESSIONID in body = %v, want the session from login", got)
	}
}

// The session must not be written into the caller's parameter map.
func TestCallDoesNotMutateParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/API/Core/Login" {
			_, _ = w.Write([]byte(`{"sessionID":"sess-1","success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"Status":"Success"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Username: "backup", Password: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"message": "save-off"}
	if err := c.Call(context.Background(), "Core", "SendConsoleMessage", params, nil); err != nil {
		t.Fatal(err)
	}
	if _, leaked := params["SESSIONID"]; leaked {
		t.Error("the session leaked into the caller's params map")
	}
}

// The panel and the disk disagree about an instance's name -- AMP shows the
// friendly name and stores files under the other -- so both have to resolve,
// and neither may be case-sensitive.
func TestResolveInstanceRootAcceptsEitherName(t *testing.T) {
	srv := instanceLister(t, []map[string]any{{
		"InstanceName": "SebsModpackv401",
		"FriendlyName": "SebsModpackv4",
		"BasePath":     "/home/amp/.ampdata/instances/SebsModpackv401",
	}})
	defer srv.Close()

	client := testClient(t, srv.URL)
	for _, name := range []string{"SebsModpackv401", "SebsModpackv4", "sebsmodpackv4"} {
		got, err := client.ResolveInstanceRoot(context.Background(), name)
		if err != nil {
			t.Fatalf("ResolveInstanceRoot(%q): %v", name, err)
		}
		if want := "/home/amp/.ampdata/instances/SebsModpackv401"; got != want {
			t.Errorf("ResolveInstanceRoot(%q) = %q, want %q", name, got, want)
		}
	}
}

// AMP has spelled the directory field differently across versions. Whichever
// one it populated is the answer; an empty one must ask for --root rather than
// hand back a path that was never there.
func TestResolveInstanceRootWithoutADirectory(t *testing.T) {
	srv := instanceLister(t, []map[string]any{{
		"InstanceName": "Quiet",
		"FriendlyName": "Quiet",
	}})
	defer srv.Close()

	_, err := testClient(t, srv.URL).ResolveInstanceRoot(context.Background(), "Quiet")
	if err == nil {
		t.Fatal("expected an error when AMP reports no directory")
	}
	if !strings.Contains(err.Error(), "--root") {
		t.Errorf("error should say how to recover, got: %v", err)
	}
}

// Naming the instances AMP does know turns "it does not work" into "you meant
// the other one", which is the difference between a two-minute and a two-hour
// misconfiguration.
func TestResolveInstanceRootListsWhatItKnows(t *testing.T) {
	srv := instanceLister(t, []map[string]any{
		{"InstanceName": "First", "BasePath": "/a"},
		{"InstanceName": "Second", "BasePath": "/b"},
	})
	defer srv.Close()

	_, err := testClient(t, srv.URL).ResolveInstanceRoot(context.Background(), "Third")
	if err == nil {
		t.Fatal("expected an error for an unknown instance")
	}
	for _, name := range []string{"First", "Second"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should list %q, got: %v", name, err)
		}
	}
}

// instanceLister serves a login and one ADSModule.GetLocalInstances reply.
func instanceLister(t *testing.T, instances []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/API/Core/Login" {
			_, _ = w.Write([]byte(`{"sessionID":"sess-1","success":true}`))
			return
		}
		if err := json.NewEncoder(w).Encode(instances); err != nil {
			t.Errorf("encoding instances: %v", err)
		}
	}))
}

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(Config{BaseURL: baseURL, Username: "user", Password: "pw"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}
