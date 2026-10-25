package requester

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeSolver records the cmds it receives and answers like FlareSolverr or
// like Byparr (which redirects / to /docs and ignores "cmd").
type fakeSolver struct {
	mu       sync.Mutex
	cmds     []string
	sessions []any
}

func (f *fakeSolver) handler(byparr bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && r.Method == http.MethodGet {
			if byparr {
				http.Redirect(w, r, "/docs", http.StatusMovedPermanently)
				return
			}
			_, _ = w.Write([]byte(`{"msg":"FlareSolverr is ready!","version":"3.3.21"}`))
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		cmd, _ := body["cmd"].(string)
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		f.sessions = append(f.sessions, body["session"])
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case byparr || cmd == "request.get":
			_, _ = w.Write([]byte(`{"status":"ok","message":"","solution":{"url":"https://x","status":200,"response":"<html><body>ok</body></html>"}}`))
		case cmd == "sessions.list":
			_, _ = w.Write([]byte(`{"status":"ok","sessions":[]}`))
		case cmd == "sessions.create":
			_, _ = w.Write([]byte(`{"status":"ok","session":"s-1"}`))
		}
	}
}

func (f *fakeSolver) seen(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if c == cmd {
			n++
		}
	}
	return n
}

func Test_flareSolverrByparrIsSessionless(t *testing.T) {
	fake := &fakeSolver{}
	srv := httptest.NewServer(fake.handler(true))
	defer srv.Close()

	f := NewFlareSolverr(srv.URL, 5000, 3)
	if f.SupportsSessions() || f.SessionsAvailable() != 3 {
		t.Fatalf("byparr: sessions=%v slots=%d", f.SupportsSessions(), f.SessionsAvailable())
	}
	if fake.seen("sessions.list")+fake.seen("sessions.create") != 0 {
		t.Fatalf("sessions.* must not be sent to Byparr: %v", fake.cmds)
	}
	body, err := f.Get(t.Context(), "https://example.com/", 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if fake.sessions[len(fake.sessions)-1] != nil {
		t.Errorf("request.get should carry no session for Byparr: %v", fake.sessions)
	}
	f.healthCheck() // no-op for slots
	if fake.seen("sessions.list") != 0 {
		t.Error("health check must not list sessions on Byparr")
	}
}

func Test_flareSolverrUsesSessions(t *testing.T) {
	fake := &fakeSolver{}
	srv := httptest.NewServer(fake.handler(false))
	defer srv.Close()

	f := NewFlareSolverr(srv.URL, 5000, 1)
	if !f.SupportsSessions() || fake.seen("sessions.create") != 1 {
		t.Fatalf("flaresolverr: sessions=%v cmds=%v", f.SupportsSessions(), fake.cmds)
	}
	body, err := f.Get(t.Context(), "https://example.com/", 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if fake.sessions[len(fake.sessions)-1] != "s-1" {
		t.Errorf("request.get should use the pooled session: %v", fake.sessions)
	}
}

func Test_flareSolverrSessionsEnvOverride(t *testing.T) {
	t.Setenv("FLARESOLVERR_SESSIONS", "false")
	fake := &fakeSolver{}
	srv := httptest.NewServer(fake.handler(false))
	defer srv.Close()

	f := NewFlareSolverr(srv.URL, 5000, 2)
	if f.SupportsSessions() || fake.seen("sessions.create") != 0 {
		t.Fatalf("FLARESOLVERR_SESSIONS=false: sessions=%v cmds=%v", f.SupportsSessions(), fake.cmds)
	}
}
