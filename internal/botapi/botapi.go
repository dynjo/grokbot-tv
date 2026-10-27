// Package botapi talks to the user's Grok Bot agents (the hosted agents of
// the Grok Bot app), so a request spoken at the TV can go to one of them.
//
// There is no public API. This uses the same calls as the community
// grok-bot-cli (gbot): the account's access token is exchanged for the
// gateway of the account's cloud computer (EnsureSandBox, which also wakes
// it), and the gateway serves sendPrompt and getAgentTranscriptTail.
package botapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var backend = "https://api2.cursor.sh"

// ErrSignedOut means the backend refused the access token: it has expired
// or was revoked, and a fresh one has to be copied from the Grok Bot app.
var ErrSignedOut = errors.New("grok bot sign-in expired or revoked")

// Client calls one account's Grok Bot agents.
type Client struct {
	token   string // the Grok Bot app's access token
	version string // the app version to report; old versions are refused
	http    *http.Client

	mu sync.Mutex
	gw *gateway
}

type gateway struct {
	url     string
	token   string
	headers map[string]string
}

// New returns a client for an access token taken from the Grok Bot app.
// appVersion is the app's version (for example "0.77.0").
func New(accessToken, appVersion string) *Client {
	return &Client{token: accessToken, version: appVersion, http: &http.Client{Timeout: 60 * time.Second}}
}

// connect returns the gateway, asking the backend for it the first time.
func (c *Client) connect(ctx context.Context) (*gateway, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gw != nil {
		return c.gw, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", backend+"/aiserver.v1.GrokBotService/EnsureSandBox", strings.NewReader("{}"))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("authorization", "Bearer "+c.token)
	req.Header.Set("x-cursor-client-type", "sand")
	req.Header.Set("x-cursor-client-version", c.version)
	req.Header.Set("x-sand-box-namespace", "prod")
	var body struct {
		GatewayURL     string            `json:"gatewayUrl"`
		GatewayToken   string            `json:"gatewayToken"`
		GatewayHeaders map[string]string `json:"gatewayHeaders"`
		NetworkToken   string            `json:"networkToken"`
		AnyrunToken    string            `json:"anyrunNetworkToken"`
		Message        string            `json:"message"`
	}
	status, err := c.do(req, &body)
	if err != nil {
		return nil, fmt.Errorf("EnsureSandBox: %w", err)
	}
	if status == 401 || status == 403 {
		if strings.Contains(body.Message, "version") {
			return nil, fmt.Errorf("EnsureSandBox: %s (update client_version)", body.Message)
		}
		return nil, ErrSignedOut
	}
	if status != 200 || body.GatewayURL == "" || body.GatewayToken == "" {
		return nil, fmt.Errorf("EnsureSandBox: HTTP %d %s", status, body.Message)
	}
	gw := &gateway{url: strings.TrimSuffix(body.GatewayURL, "/"), token: body.GatewayToken, headers: map[string]string{}}
	for k, v := range body.GatewayHeaders {
		gw.headers[strings.ToLower(k)] = v
	}
	if body.NetworkToken == "" {
		body.NetworkToken = body.AnyrunToken
	}
	if len(gw.headers) == 0 && body.NetworkToken != "" {
		gw.headers["x-anyrun-network-token"] = body.NetworkToken
	}
	c.gw = gw
	return gw, nil
}

// call runs a gateway method. If the gateway refuses the call or can't be
// reached, it is looked up again once: the cloud computer may have been
// recreated or gone to sleep.
func (c *Client) call(ctx context.Context, method string, in, out any) error {
	buf, err := json.Marshal(in)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		gw, err := c.connect(ctx)
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", gw.url+"/api/"+method, bytes.NewReader(buf))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", "Bearer "+gw.token)
		for k, v := range gw.headers {
			req.Header.Set(k, v)
		}
		status, err := c.do(req, out)
		retry := err != nil || status == 401 || status == 403 || status == 502 || status == 503
		// Never send a prompt twice: it may have arrived.
		if retry && attempt == 0 && ctx.Err() == nil && (method != "sendPrompt" || status == 401 || status == 403) {
			c.mu.Lock()
			c.gw = nil
			c.mu.Unlock()
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		if status != 200 {
			return fmt.Errorf("%s: HTTP %d", method, status)
		}
		return nil
	}
}

func (c *Client) do(req *http.Request, out any) (int, error) {
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return res.StatusCode, err
	}
	if len(body) > 0 && out != nil {
		if err := json.Unmarshal(body, out); err != nil && res.StatusCode == 200 {
			return res.StatusCode, fmt.Errorf("bad reply: %v", err)
		}
	}
	return res.StatusCode, nil
}

// Send posts prompt to the agent's thread as the user. clientNonce marks the
// message so its entry can be found in the transcript.
func (c *Client) Send(ctx context.Context, agentID, prompt, clientNonce string) error {
	var out struct {
		MessageID string `json:"messageId"`
	}
	return c.call(ctx, "sendPrompt", map[string]any{"agentId": agentID, "prompt": prompt, "clientNonce": clientNonce}, &out)
}

// Entry is one transcript entry. The user's messages are kind "message"
// with role "user"; what the agent says to the user is kind
// "send-message", with the requestId of the message it answers.
type Entry struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"`
	Seq         int64     `json:"seq"`
	Role        string    `json:"role"`
	ClientNonce string    `json:"clientNonce"`
	RequestID   string    `json:"requestId"`
	Message     *Outgoing `json:"message"`
}

// Outgoing is the body of a send-message entry.
type Outgoing struct {
	Type    string `json:"type"` // "text" or "widget"
	Content string `json:"content"`
	Widget  *struct {
		Prompt  string `json:"prompt"`
		Options []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"widget"`
}

// Text is what the entry says to the user, or "" if it says nothing.
// A widget (a multiple-choice question) becomes its question and options.
func (e Entry) Text() string {
	if e.Kind != "send-message" || e.Message == nil {
		return ""
	}
	switch e.Message.Type {
	case "text":
		return strings.TrimSpace(e.Message.Content)
	case "widget":
		w := e.Message.Widget
		if w == nil {
			return ""
		}
		var opts []string
		for _, o := range w.Options {
			opts = append(opts, o.Label)
		}
		text := strings.TrimSpace(w.Prompt)
		if len(opts) > 0 {
			text += " " + strings.Join(opts, ", or ") + "?"
		}
		return text
	}
	return ""
}

// Tail returns the agent's last limit (1–200) transcript entries.
func (c *Client) Tail(ctx context.Context, agentID string, limit int) ([]Entry, error) {
	var out struct {
		Entries []Entry `json:"entries"`
	}
	err := c.call(ctx, "getAgentTranscriptTail", map[string]any{"id": agentID, "limit": limit}, &out)
	return out.Entries, err
}

// Warm connects to the gateway ahead of the first call, waking the cloud
// computer if it is asleep.
func (c *Client) Warm(ctx context.Context) error {
	_, err := c.connect(ctx)
	return err
}
