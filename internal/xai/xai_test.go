package xai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamsTextAndToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true || len(req["tools"].([]any)) != 1 {
			t.Errorf("request %v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"choices":[{"delta":{"role":"assistant","content":"Open"}}]}`,
			`{"choices":[{"delta":{"content":"ing"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"open_app","arguments":"{\"na"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"me\":\"Netflix\"}"}}]}}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", d)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	var streamed strings.Builder
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "open_app"}}}
	msg, err := c.Chat(context.Background(), "m", []Message{{Role: "user", Content: "hi"}}, tools, func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Opening" || streamed.String() != "Opening" {
		t.Errorf("content %q streamed %q", msg.Content, streamed.String())
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "c1" || msg.ToolCalls[0].Function.Arguments != `{"name":"Netflix"}` {
		t.Errorf("tool calls %+v", msg.ToolCalls)
	}

	c.APIKey = "wrong"
	_, err = c.Chat(context.Background(), "m", nil, nil, nil)
	if e, ok := err.(*Error); !ok || e.Status != 401 {
		t.Errorf("want 401 Error, got %v", err)
	}
}

func TestWAVHeader(t *testing.T) {
	w := WAV([]int16{1, -1}, 16000)
	if len(w) != 48 || string(w[:4]) != "RIFF" || string(w[36:40]) != "data" {
		t.Errorf("bad wav header % x", w[:44])
	}
}

func TestImageMessage(t *testing.T) {
	buf, _ := json.Marshal(Message{Role: "user", Content: "look", Images: [][]byte{{0xff, 0xd8}}})
	want := `{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9g=","detail":"high"}}]}`
	if string(buf) != want {
		t.Errorf("got %s", buf)
	}
	buf, _ = json.Marshal(Message{Role: "user", Content: "hi"})
	if string(buf) != `{"role":"user","content":"hi"}` {
		t.Errorf("plain message: %s", buf)
	}
}
