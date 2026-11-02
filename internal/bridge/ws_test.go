package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWSKey(t *testing.T) {
	h := NewHub()
	srv := httptest.NewServer(h.mux)
	defer srv.Close()
	for _, tc := range []struct {
		origin, key string
		want        int
	}{
		{"file://", h.Key, http.StatusSwitchingProtocols},
		{"null", h.Key, http.StatusSwitchingProtocols},
		{"null", "", http.StatusForbidden},
		{"file://", "wrong", http.StatusForbidden},
		{"http://evil.example", h.Key, http.StatusForbidden},
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/?key="+tc.key, nil)
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		req.Header.Set("Sec-WebSocket-Version", "13")
		res, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("origin %q key %q: status %d, want %d", tc.origin, tc.key, res.StatusCode, tc.want)
		}
	}
}
