// Package xai talks to the xAI API: speech-to-text, streaming chat
// completions with tool calls, and text-to-speech.
package xai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.x.ai/v1"

// Client is an xAI API client.
type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

// New returns a client. webOS often has slow or broken IPv6 routes, so it
// dials IPv4 only.
func New(apiKey string) *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Client{APIKey: apiKey, BaseURL: defaultBaseURL, HTTP: &http.Client{Transport: tr}}
}

// Error is a non-2xx API response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("xai: HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &Error{Status: resp.StatusCode, Body: string(body)}
	}
	return resp, nil
}

// WAV wraps 16-bit mono PCM in a WAV header.
func WAV(pcm []int16, rate int) []byte {
	n := len(pcm) * 2
	le := binary.LittleEndian
	b := make([]byte, 0, 44+n)
	b = le.AppendUint32(append(b, "RIFF"...), uint32(36+n))
	b = append(b, "WAVEfmt "...)
	b = le.AppendUint32(b, 16)           // fmt chunk size
	b = le.AppendUint16(b, 1)            // PCM
	b = le.AppendUint16(b, 1)            // mono
	b = le.AppendUint32(b, uint32(rate)) // sample rate
	b = le.AppendUint32(b, uint32(rate*2))
	b = le.AppendUint16(b, 2) // block align
	b = le.AppendUint16(b, 16)
	b = le.AppendUint32(append(b, "data"...), uint32(n))
	for _, s := range pcm {
		b = le.AppendUint16(b, uint16(s))
	}
	return b
}

// Transcribe sends 16 kHz PCM to /v1/stt and returns the text.
func (c *Client) Transcribe(ctx context.Context, model, language string, pcm []int16) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("model", model)
	if language != "" {
		mw.WriteField("language", language)
	}
	mw.WriteField("format", "true")
	fw, err := mw.CreateFormFile("file", "speech.wav")
	if err != nil {
		return "", err
	}
	fw.Write(WAV(pcm, 16000))
	mw.Close()
	req, err := http.NewRequest("POST", c.BaseURL+"/stt", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.do(ctx, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Text       string `json:"text"`
		Transcript string `json:"transcript"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("xai: stt response: %w", err)
	}
	if out.Text == "" {
		out.Text = out.Transcript
	}
	return strings.TrimSpace(out.Text), nil
}

// Message is a chat message in the OpenAI-compatible format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Images     [][]byte   `json:"-"` // JPEG images sent along with Content
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// MarshalJSON sends a message with images as content parts.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}
	type imageURL struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	type part struct {
		Type     string    `json:"type"`
		Text     string    `json:"text,omitempty"`
		ImageURL *imageURL `json:"image_url,omitempty"`
	}
	parts := []part{{Type: "text", Text: m.Content}}
	for _, img := range m.Images {
		url := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img)
		parts = append(parts, part{Type: "image_url", ImageURL: &imageURL{URL: url, Detail: "high"}})
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content []part `json:"content"`
	}{m.Role, parts})
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is a tool's name, description and JSON Schema parameters.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Chat streams a chat completion. onText receives content deltas as they
// arrive; the returned message holds the full content and any tool calls.
func (c *Client) Chat(ctx context.Context, model string, msgs []Message, tools []Tool, onText func(string)) (Message, error) {
	payload := map[string]any{"model": model, "messages": msgs, "stream": true}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequest("POST", c.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.do(ctx, req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()

	out := Message{Role: "assistant"}
	var content strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			content.WriteString(d.Content)
			if onText != nil {
				onText(d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			for len(out.ToolCalls) <= tc.Index {
				out.ToolCalls = append(out.ToolCalls, ToolCall{Type: "function"})
			}
			call := &out.ToolCalls[tc.Index]
			if tc.ID != "" {
				call.ID = tc.ID
			}
			call.Function.Name += tc.Function.Name
			call.Function.Arguments += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, err
	}
	out.Content = content.String()
	return out, nil
}

// Speak synthesizes text with /v1/tts and returns MP3 audio.
func (c *Client) Speak(ctx context.Context, voice, language, text string) ([]byte, error) {
	body := map[string]any{"text": text, "voice_id": voice}
	if language != "" {
		body["language"] = language
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", c.BaseURL+"/tts", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
