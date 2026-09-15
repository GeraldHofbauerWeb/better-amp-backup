package amp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
