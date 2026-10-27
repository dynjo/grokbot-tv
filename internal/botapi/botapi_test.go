package botapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEntryText(t *testing.T) {
	var e Entry
	json.Unmarshal([]byte(`{"kind":"send-message","message":{"type":"widget","widget":{"prompt":"Which day?","options":[{"label":"Wed"},{"label":"Fri"}]}}}`), &e)
	if got := e.Text(); got != "Which day? Wed, or Fri?" {
		t.Errorf("widget text = %q", got)
	}
	json.Unmarshal([]byte(`{"kind":"message","role":"user","content":"hi"}`), &e)
	if got := e.Text(); got != "" {
		t.Errorf("user message text = %q", got)
	}
}

// fakeBot is a gateway whose agent answers each prompt twice.
type fakeBot struct {
	mu      sync.Mutex
	entries []map[string]any
	ensures int
}

func (f *fakeBot) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	switch r.URL.Path {
	case "/aiserver.v1.GrokBotService/EnsureSandBox":
		if r.Header.Get("authorization") != "Bearer app-token" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"message": "unauthenticated"})
			return
		}
		f.ensures++
		json.NewEncoder(w).Encode(map[string]any{"gatewayUrl": "http://" + r.Host + "/gw/", "gatewayToken": "gw-token", "gatewayHeaders": map[string]string{"X-Route": "r1"}})
	case "/gw/api/sendPrompt":
		if r.Header.Get("authorization") != "Bearer gw-token" || r.Header.Get("x-route") != "r1" {
			w.WriteHeader(403)
			return
		}
		f.entries = append(f.entries,
			map[string]any{"kind": "message", "id": "u1", "role": "user", "content": in["prompt"], "clientNonce": in["clientNonce"], "requestId": "req1"},
			map[string]any{"kind": "send-message", "id": "s0", "requestId": "other", "message": map[string]any{"type": "text", "content": "unrelated"}},
			map[string]any{"kind": "send-message", "id": "s1", "requestId": "req1", "message": map[string]any{"type": "text", "content": "On it."}},
		)
		json.NewEncoder(w).Encode(map[string]any{"messageId": "u1"})
		go func() {
			time.Sleep(30 * time.Millisecond)
			f.mu.Lock()
			f.entries = append(f.entries, map[string]any{"kind": "send-message", "id": "s2", "requestId": "req1", "message": map[string]any{"type": "text", "content": "Dentist at 1:30."}})
			f.mu.Unlock()
		}()
	case "/gw/api/getAgentTranscriptTail":
		json.NewEncoder(w).Encode(map[string]any{"entries": f.entries})
	default:
		w.WriteHeader(404)
	}
}

func TestAsk(t *testing.T) {
	f := &fakeBot{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	backend = srv.URL
	pollEvery, quietFor, waitFor = 10*time.Millisecond, 100*time.Millisecond, time.Second

	var got []string
	c := New("app-token", "0.77.0")
	if err := c.Ask(context.Background(), "agent", "what's on tomorrow?", func(s string) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "On it.|Dentist at 1:30." {
		t.Errorf("replies = %q", got)
	}
	if f.ensures != 1 {
		t.Errorf("EnsureSandBox called %d times", f.ensures)
	}

	if err := New("old-token", "0.77.0").Send(context.Background(), "agent", "hi", "n"); err != ErrSignedOut && !strings.Contains(err.Error(), ErrSignedOut.Error()) {
		t.Errorf("expired token: %v", err)
	}
}

// TestLiveTail reads an agent's transcript (read-only):
// BOTAPI_TOKEN=… BOTAPI_AGENT=… BOTAPI_VERSION=0.77.0 go test -run LiveTail
func TestLiveTail(t *testing.T) {
	token, agent := os.Getenv("BOTAPI_TOKEN"), os.Getenv("BOTAPI_AGENT")
	if token == "" || agent == "" {
		t.Skip("BOTAPI_TOKEN and BOTAPI_AGENT not set")
	}
	backend = "https://api2.cursor.sh"
	t0 := time.Now()
	c := New(token, os.Getenv("BOTAPI_VERSION"))
	entries, err := c.Tail(context.Background(), agent, 10)
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()
	if _, err := c.Tail(context.Background(), agent, 10); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d entries; first read %v (with EnsureSandBox), second %v", len(entries), t1.Sub(t0).Round(time.Millisecond), time.Since(t1).Round(time.Millisecond))
	for _, e := range entries {
		t.Logf("seq %d %s role=%s req=%.8s nonce=%.8s text=%.40q", e.Seq, e.Kind, e.Role, e.RequestID, e.ClientNonce, e.Text())
	}
}
