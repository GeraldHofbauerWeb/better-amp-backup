// Package fake provides an in-process stand-in for an AMP panel.
//
// The quiesce path is the one piece of this tool that can harm a running
// server, so it has to be testable without one: every failure mode that
// matters — a console that never confirms, a session that expires mid-run, a
// panel that goes away entirely — is reproducible here and nowhere else.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
)

// Server is a fake AMP panel.
type Server struct {
	*httptest.Server

	mu sync.Mutex

	username string
	password string
	sessions map[string]bool

	// Commands records every console message received, in order.
	Commands []string
	// pending holds console lines not yet drained by GetUpdates.
	pending []string
	// state is the reported application state.
	state int

	// Responder, when set, may take over any call. Returning handled=false
	// falls through to the default behaviour.
	Responder func(module, method string, params map[string]any) (body any, handled bool)

	// ExpireSessionsOnce makes the next authenticated call fail as if the
	// session had lapsed, exactly once.
	ExpireSessionsOnce bool

	// SaveConfirmation is the line emitted in reply to a flush command.
	SaveConfirmation string
	// flushPattern decides which command triggers that line.
	flushPattern *regexp.Regexp
	// SilentFlush suppresses the confirmation, to test the timeout path.
	SilentFlush bool

	// Logins counts successful Core.Login calls.
	Logins int

	// InstanceID, when set, makes this fake behave like a controller that
	// proxies to one instance: calls arriving under
	// /API/ADSModule/Servers/<InstanceID>/API/... are served, and calls to any
	// other instance id are refused the way AMP refuses an unknown one.
	InstanceID string
	// RequireProxy refuses calls that do not go through the instance proxy, so
	// a test can prove a client actually routed through it.
	RequireProxy bool

	// Spec is what Core.GetAPISpec returns. AMP filters this by the caller's
	// permissions, which is why the tool reads capabilities out of it rather
	// than hard-coding method names -- so a test needs to be able to hand back
	// a smaller spec and see the caller adapt.
	Spec map[string]map[string]any

	// ProxiedCalls records every call that arrived through the instance proxy.
	ProxiedCalls []string
}

// New starts a fake panel. Close it with Close.
func New(username, password string) *Server {
	s := &Server{
		username:         username,
		password:         password,
		sessions:         map[string]bool{},
		state:            20, // ready
		SaveConfirmation: "[Server thread/INFO]: Saved the game",
		flushPattern:     regexp.MustCompile(`save-all`),
		Spec:             DefaultSpec(),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// DefaultSpec is the API surface a fully privileged session sees.
func DefaultSpec() map[string]map[string]any {
	return map[string]map[string]any{
		"Core": {
			"Login":              map[string]any{},
			"GetStatus":          map[string]any{},
			"GetUpdates":         map[string]any{},
			"SendConsoleMessage": map[string]any{},
			"GetAPISpec":         map[string]any{},
			"Start":              map[string]any{},
			"Stop":               map[string]any{},
			"Restart":            map[string]any{},
		},
		"ADSModule": {"GetLocalInstances": map[string]any{}},
		"LocalFileBackupPlugin": {
			"GetBackups":        map[string]any{},
			"RefreshBackupList": map[string]any{},
		},
	}
}

// BackupAccountSpec is what the deliberately narrow service account sees: it
// may read and write the console, and nothing else. Notably it may not start
// or stop the application, which is the property the deployment relies on.
func BackupAccountSpec() map[string]map[string]any {
	return map[string]map[string]any{
		"Core": {
			"Login":              map[string]any{},
			"GetStatus":          map[string]any{},
			"GetUpdates":         map[string]any{},
			"SendConsoleMessage": map[string]any{},
			"GetAPISpec":         map[string]any{},
		},
	}
}

// AddSession registers a session id without a login, standing in for one the
// panel issued to a person and handed to us.
func (s *Server) AddSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = true
}

// RevokeSession drops a session, as AMP does when it expires.
func (s *Server) RevokeSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// ProxiedCallLog returns a copy of the calls that arrived through the proxy.
func (s *Server) ProxiedCallLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ProxiedCalls...)
}

// SetState changes the reported application state.
func (s *Server) SetState(state int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

// CommandLog returns a copy of the console commands received so far.
func (s *Server) CommandLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Commands...)
}

// EmitConsole queues a console line for the next GetUpdates.
func (s *Server) EmitConsole(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, line)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 || parts[0] != "API" {
		http.NotFound(w, r)
		return
	}

	// AMP's controller proxies to an instance under
	// /API/ADSModule/Servers/<id>/API/<Module>/<Method>. The panel uses that
	// path for everything once you open an instance, so the double has to
	// serve it -- and has to refuse an id it does not host, because a client
	// pointed at the wrong instance otherwise looks like it is working.
	proxied := false
	if len(parts) == 7 && parts[1] == "ADSModule" && parts[2] == "Servers" && parts[4] == "API" {
		if s.InstanceID == "" || parts[3] != s.InstanceID {
			writeJSON(w, map[string]any{"Title": "Unauthorized Access", "Status": false,
				"Message": fmt.Sprintf("No instance with ID %s", parts[3])})
			return
		}
		proxied = true
		parts = []string{"API", parts[5], parts[6]}
	}
	if len(parts) != 3 {
		http.NotFound(w, r)
		return
	}
	module, method := parts[1], parts[2]

	if s.RequireProxy && !proxied {
		writeJSON(w, map[string]any{"Title": "Unauthorized Access", "Status": false,
			"Message": "this endpoint is only reachable through the instance proxy"})
		return
	}
	if proxied {
		s.mu.Lock()
		s.ProxiedCalls = append(s.ProxiedCalls, module+"."+method)
		s.mu.Unlock()
	}

	params := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&params)
	}

	s.mu.Lock()
	if s.Responder != nil {
		responder := s.Responder
		s.mu.Unlock()
		if body, handled := responder(module, method, params); handled {
			writeJSON(w, body)
			return
		}
		s.mu.Lock()
	}

	if module == "Core" && method == "Login" {
		s.handleLogin(w, params)
		s.mu.Unlock()
		return
	}

	// Everything else needs a session, and AMP takes it as a field in the
	// request body -- not as a header. Reading it from a header here once made
	// this double agree with a client bug that the real panel did not share:
	// the calls "succeeded" against the fake and came back anonymous in
	// production. A test double must follow the protocol, not the caller.
	session, _ := params["SESSIONID"].(string)
	if r.Header.Get("SESSIONID") != "" {
		writeJSON(w, map[string]any{"Title": "Unauthorized Access", "Status": false,
			"Message": "SESSIONID was sent as a header; AMP ignores that"})
		s.mu.Unlock()
		return
	}
	if s.ExpireSessionsOnce {
		s.ExpireSessionsOnce = false
		delete(s.sessions, session)
	}
	if !s.sessions[session] {
		s.mu.Unlock()
		writeJSON(w, map[string]any{"Title": "Unauthorized Access", "Status": false})
		return
	}

	body := s.dispatch(module, method, params)
	s.mu.Unlock()
	writeJSON(w, body)
}

// handleLogin runs with the mutex held.
func (s *Server) handleLogin(w http.ResponseWriter, params map[string]any) {
	user, _ := params["username"].(string)
	pass, _ := params["password"].(string)
	if user != s.username || pass != s.password {
		writeJSON(w, map[string]any{"success": false, "resultReason": "Incorrect username or password"})
		return
	}
	s.Logins++
	id := fmt.Sprintf("session-%d", s.Logins)
	s.sessions[id] = true
	writeJSON(w, map[string]any{"sessionID": id, "success": true})
}

// dispatch runs with the mutex held.
func (s *Server) dispatch(module, method string, params map[string]any) any {
	switch module + "." + method {
	case "Core.GetStatus":
		return map[string]any{"State": s.state, "Uptime": "01:00:00"}

	case "Core.SendConsoleMessage":
		msg, _ := params["message"].(string)
		s.Commands = append(s.Commands, msg)
		if s.flushPattern.MatchString(msg) && !s.SilentFlush {
			s.pending = append(s.pending, s.SaveConfirmation)
		}
		return map[string]any{"Status": "Success"}

	case "Core.GetUpdates":
		entries := make([]map[string]any, 0, len(s.pending))
		for _, line := range s.pending {
			entries = append(entries, map[string]any{
				"Timestamp": "2026-09-07T18:00:00Z",
				"Source":    "Server thread",
				"Type":      "Console",
				"Contents":  line,
			})
		}
		s.pending = nil
		return map[string]any{
			"ConsoleEntries": entries,
			"Status":         map[string]any{"State": s.state},
		}

	case "ADSModule.GetLocalInstances":
		return []map[string]any{{
			"InstanceID":   "6b85d8b3-eaff-4722-ac97-52e4f4441948",
			"InstanceName": "SebsModpackv401",
			"FriendlyName": "SebsModpackv4",
			"Module":       "MinecraftModule",
			"Running":      true,
			"BasePath":     "/home/amp/.ampdata/instances/SebsModpackv401",
		}}

	case "Core.GetAPISpec":
		return s.Spec

	case "Core.Start", "Core.Stop", "Core.Restart":
		// AMP filters the spec by permission, so a method missing from it is
		// one this session may not call. The fake enforces that, or a test
		// would pass against a panel that would have refused.
		if _, ok := s.Spec["Core"][method]; !ok {
			return map[string]any{"Title": "Unauthorized Access", "Status": false,
				"Message": "You do not have permission to use this method at this time."}
		}
		// Stopping is a process, not an event: AMP returns immediately and the
		// application settles afterwards. The state moves one step here so a
		// caller that does not wait sees the intermediate value it deserves.
		switch method {
		case "Start":
			s.state = 10 // starting
		case "Stop", "Restart":
			s.state = 40 // stopping
		}
		return map[string]any{"Status": "Success"}
	}
	return map[string]any{"Status": "Success"}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
