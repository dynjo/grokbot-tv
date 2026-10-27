package botapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// ErrNoReply means the agent didn't answer within the wait.
var ErrNoReply = errors.New("no reply")

// Timing of Ask. The agent often says "on it" first and the result later,
// so a reply only ends the exchange after a quiet spell.
var (
	pollEvery = time.Second
	quietFor  = 45 * time.Second // after the last reply
	waitFor   = 5 * time.Minute  // for the first reply
)

// Ask sends prompt to the agent and calls onReply with each thing it says
// in answer, in order, until it has been quiet for a while. It returns
// ErrNoReply if nothing came back.
func (c *Client) Ask(ctx context.Context, agentID, prompt string, onReply func(string)) error {
	nonce := uuid()
	if err := c.Send(ctx, agentID, prompt, nonce); err != nil {
		return err
	}
	var (
		requestID string
		seen      = map[string]bool{}
		start     = time.Now()
		last      time.Time // last reply
		failures  int
	)
	for {
		// Poll quickly while an answer is likely, then back off.
		wait := pollEvery
		if time.Since(start) > time.Minute {
			wait = 3 * pollEvery
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		entries, err := c.Tail(ctx, agentID, 200)
		if err != nil {
			if failures++; failures >= 5 || errors.Is(err, ErrSignedOut) {
				return err
			}
			continue
		}
		failures = 0
		// The message we sent carries our nonce; the agent's answers carry
		// its request id.
		for _, e := range entries {
			if requestID == "" && e.ClientNonce == nonce && e.RequestID != "" {
				requestID = e.RequestID
			}
		}
		for _, e := range entries {
			if requestID == "" || e.RequestID != requestID || seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			if text := e.Text(); text != "" {
				onReply(text)
				last = time.Now()
			}
		}
		switch {
		case !last.IsZero() && time.Since(last) > quietFor:
			return nil
		case last.IsZero() && time.Since(start) > waitFor:
			return ErrNoReply
		}
	}
}

func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
