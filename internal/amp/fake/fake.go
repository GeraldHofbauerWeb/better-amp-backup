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
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
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
	if len(parts) != 3 || parts[0] != "API" {
		http.NotFound(w, r)
		return
	}
	module, method := parts[1], parts[2]

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

	// Everything else needs a session.
	session := r.Header.Get("SESSIONID")
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
		return map[string]any{
			"Core": map[string]any{
				"Login":              map[string]any{},
				"GetStatus":          map[string]any{},
				"GetUpdates":         map[string]any{},
				"SendConsoleMessage": map[string]any{},
				"GetAPISpec":         map[string]any{},
			},
			"ADSModule": map[string]any{"GetLocalInstances": map[string]any{}},
			"LocalFileBackupPlugin": map[string]any{
				"GetBackups":        map[string]any{},
				"RefreshBackupList": map[string]any{},
			},
		}
	}
	return map[string]any{"Status": "Success"}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
