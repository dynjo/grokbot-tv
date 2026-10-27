package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"grokbot/internal/botapi"
	"grokbot/internal/bridge"
	"grokbot/internal/xai"
)

// With grok_bot configured, the TV is a voice front end for the user's own
// Grok Bot agent: everything the user says goes to the agent, and everything
// the agent says is shown and read out. xAI only turns speech into text and
// back. The agent controls the TV by writing commands such as
// [[tv volume 30]] in its messages, which the TV carries out instead of
// reading them out.

// botIntro starts the first message of each conversation at the TV.
const botIntro = `[You are talking by voice through the user's LG TV: this was said to the TV, and your messages are shown and read aloud there. Reply in one to three short spoken sentences, with no markdown, lists, links or emoji. You can control the TV by writing a command on its own line: [[tv open Netflix]] opens an app, [[tv input hdmi1]] switches input (hdmi1 to hdmi4, or tv for live TV), [[tv volume 30]] sets the volume, [[tv volume +5]] or [[tv volume -5]] changes it, [[tv mute]] and [[tv unmute]], and [[tv look]] sends you a description of what is on screen. Use them only when the user asks. They are carried out, not read out.]`

// botTag marks the rest of the conversation's messages.
const botTag = "[TV voice] "

// describePrompt asks for a description of a screenshot for [[tv look]].
const describePrompt = "This image is a screenshot of exactly what the TV is displaying right now (the picture itself, not a photo of a TV). Describe it for someone who can't see it, in two to four sentences: the app or input, what is playing, who is on screen (name them if you recognise them), and any important text."

const (
	firstReplyWait = 90 * time.Second        // for the agent's answer, with the overlay up
	moreReplyWait  = 2500 * time.Millisecond // for a further message after one
	lookReplyWait  = 40 * time.Second        // for the answer after [[tv look]]
)

// botMessage is something the agent said, waiting to be read out.
type botMessage struct {
	from, text string
}

// botAuth is the sign-in copied from the Grok Bot app.
type botAuth struct {
	AccessToken string `json:"access_token"`
	AppVersion  string `json:"app_version"`
}

func (a *Assistant) botEnabled() bool { return a.cfg.GrokBot.AgentID != "" }

func (a *Assistant) botName() string {
	if n := strings.TrimSpace(a.cfg.GrokBot.Name); n != "" {
		return n
	}
	return "Grok Bot"
}

// client returns a client for the current sign-in, which is read again on
// every request so that a fresh copy needs no restart.
func (a *Assistant) client() (*botapi.Client, error) {
	buf, err := os.ReadFile(a.cfg.GrokBot.AuthFile)
	if err != nil {
		return nil, fmt.Errorf("no Grok Bot sign-in on the TV (%v)", err)
	}
	var auth botAuth
	if err := json.Unmarshal(buf, &auth); err != nil || auth.AccessToken == "" {
		return nil, fmt.Errorf("bad Grok Bot sign-in file %s", a.cfg.GrokBot.AuthFile)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bot == nil || a.botAuth != auth {
		a.bot = botapi.New(auth.AccessToken, auth.AppVersion)
		a.botAuth = auth
	}
	return a.bot, nil
}

// warmUp connects to the agent's cloud computer ahead of the first message,
// which wakes it if it is asleep.
func (a *Assistant) warmUp() {
	if c, err := a.client(); err == nil {
		if err := c.Warm(a.runCtx); err != nil {
			log.Printf("grokbot: connect: %v", err)
		}
	}
}

// askBot sends text to the agent and delivers what it says back. It runs in
// the background for as long as the agent takes.
func (a *Assistant) askBot(text string) {
	ctx := a.runCtx
	name := a.botName()
	system := func(text string) { a.deliver(botMessage{from: "Grokbot", text: text}) }
	c, err := a.client()
	if err != nil {
		log.Printf("grokbot: %v", err)
		system(fmt.Sprintf("I can't reach %s: the TV has no Grok Bot sign-in. Run sync-grok-bot on your Mac.", name))
		return
	}
	t0 := time.Now()
	log.Printf("grokbot: to %s: %q", name, text)
	err = c.Ask(ctx, a.cfg.GrokBot.AgentID, text, func(reply string) {
		log.Printf("grokbot: from %s after %v: %q", name, time.Since(t0).Round(100*time.Millisecond), reply)
		a.deliver(botMessage{from: name, text: reply})
	})
	switch {
	case err == nil, ctx.Err() != nil:
	case errors.Is(err, botapi.ErrSignedOut):
		log.Printf("grokbot: %v", err)
		system(fmt.Sprintf("I can't reach %s: the TV's Grok Bot sign-in has expired. Run sync-grok-bot on your Mac.", name))
	case errors.Is(err, botapi.ErrNoReply):
		log.Printf("grokbot: no reply from %s", name)
		system(fmt.Sprintf("%s hasn't answered. The reply will be in the Grok Bot app.", name))
	default:
		log.Printf("grokbot: %v", err)
		system(fmt.Sprintf("Something went wrong talking to %s.", name))
	}
}

// deliver hands a message to the conversation on screen, or queues it to
// be read out when the TV is free. With the screen off it stays in the Grok
// Bot app.
func (a *Assistant) deliver(m botMessage) {
	a.mu.Lock()
	if a.live != nil {
		select {
		case a.live <- m:
			a.mu.Unlock()
			return
		default:
		}
	}
	a.mu.Unlock()
	if state := bridge.PowerState(a.runCtx); state != "Active" {
		log.Printf("grokbot: TV is %q; not reading out %s's message", state, m.from)
		return
	}
	select {
	case a.inbox <- m:
	default:
		log.Printf("grokbot: too many messages queued; dropped one from %s", m.from)
	}
}

// openLive starts routing the agent's messages to this conversation;
// closeLive stops it and queues any that weren't read out.
func (a *Assistant) openLive() <-chan botMessage {
	live := make(chan botMessage, 16)
	a.mu.Lock()
	a.live = live
	a.mu.Unlock()
	return live
}

func (a *Assistant) closeLive() {
	a.mu.Lock()
	live := a.live
	a.live = nil
	a.mu.Unlock()
	for live != nil {
		select {
		case m := <-live:
			a.deliver(m)
		default:
			return
		}
	}
}

// handOver sends what the user said to the agent and reads out its answer.
// It returns whether the conversation goes on, like turn. intro is true for
// the conversation's first message.
func (a *Assistant) handOver(ctx, tctx context.Context, text string, intro bool, live <-chan botMessage) (more bool) {
	prompt := botTag + text
	if intro {
		prompt = botIntro + "\n\n" + text
	}
	go a.askBot(prompt)
	a.hub.Send("thinking", nil, true)
	select {
	case m := <-live:
		if a.sayAll(tctx, []botMessage{m}, live) {
			a.close(ctx, 1500*time.Millisecond)
			return false
		}
		return ctx.Err() == nil
	case <-time.After(firstReplyWait):
		name := a.botName()
		a.hub.Send("message", map[string]any{"from": "Grokbot", "text": name + " is still working on it. The answer will be read out when it comes."}, true)
		a.close(ctx, 4*time.Second)
		return false
	case <-tctx.Done():
		return ctx.Err() == nil // a press: listen again; the answer comes later
	}
}

// sayAll reads out msgs, then any further messages that follow soon. It
// returns true if the agent opened an app, which ends the conversation.
// A cancelled ctx (a press) stops it.
func (a *Assistant) sayAll(ctx context.Context, msgs []botMessage, live <-chan botMessage) (launched bool) {
	for i := 0; ; i++ {
		var m botMessage
		if i < len(msgs) {
			m = msgs[i]
		} else {
			wait := moreReplyWait
			if a.looking {
				// After [[tv look]] the agent answers once it has the
				// description.
				wait = lookReplyWait
				a.hub.Send("thinking", nil, true)
			}
			select {
			case m = <-live:
			case <-time.After(wait):
				return launched
			case <-ctx.Done():
				return launched
			}
		}
		a.looking = false
		if a.say(ctx, m, i > 0) {
			launched = true
		}
		if ctx.Err() != nil || launched {
			return launched
		}
	}
}

// say shows and reads out one message and carries out its TV commands.
func (a *Assistant) say(ctx context.Context, m botMessage, appendText bool) (launched bool) {
	text, cmds := tvCommands(m.text)
	if text != "" {
		a.hub.Send("message", map[string]any{"from": m.from, "text": speakable(text), "append": appendText}, true)
		if a.cfg.TTS {
			a.speak(ctx, text)
		}
	}
	for _, cmd := range cmds {
		if ctx.Err() != nil {
			break
		}
		if a.runCommand(ctx, cmd) {
			launched = true
		}
	}
	return launched
}

var (
	tvCommand  = regexp.MustCompile(`(?i)\[\[\s*tv\s+([^\]]*?)\s*\]\]`)
	blankLines = regexp.MustCompile(`\n{3,}`)
)

// tvCommands splits a message into its text and its [[tv …]] commands.
func tvCommands(msg string) (string, []string) {
	var cmds []string
	for _, m := range tvCommand.FindAllStringSubmatch(msg, -1) {
		cmds = append(cmds, m[1])
	}
	lines := strings.Split(tvCommand.ReplaceAllString(msg, ""), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	text := strings.Join(lines, "\n")
	return strings.TrimSpace(blankLines.ReplaceAllString(text, "\n\n")), cmds
}

// commandCall turns a [[tv …]] command into the matching TV tool call.
func commandCall(cmd string) (name string, args map[string]any, ok bool) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(cmd), " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(verb) {
	case "open":
		return "open_app", map[string]any{"name": rest}, rest != ""
	case "input":
		return "switch_input", map[string]any{"input": strings.ToLower(strings.ReplaceAll(rest, " ", ""))}, rest != ""
	case "volume":
		n, err := strconv.Atoi(strings.TrimPrefix(rest, "+"))
		if err != nil {
			return "", nil, false
		}
		if strings.HasPrefix(rest, "+") || strings.HasPrefix(rest, "-") {
			return "change_volume", map[string]any{"delta": n}, true
		}
		return "set_volume", map[string]any{"level": n}, true
	case "mute":
		return "set_mute", map[string]any{"muted": true}, true
	case "unmute":
		return "set_mute", map[string]any{"muted": false}, true
	case "look":
		return "look_at_screen", nil, true
	}
	return "", nil, false
}

// runCommand carries out a [[tv …]] command. It returns true if an app now
// covers the overlay.
func (a *Assistant) runCommand(ctx context.Context, cmd string) bool {
	name, args, ok := commandCall(cmd)
	if !ok {
		log.Printf("grokbot: unknown TV command %q", cmd)
		return false
	}
	buf, _ := json.Marshal(args)
	var call xai.ToolCall
	call.Function.Name, call.Function.Arguments = name, string(buf)
	res := a.runTool(ctx, call)
	log.Printf("grokbot: [[tv %s]] -> %s", cmd, res.text)
	if res.action != "" {
		a.hub.Send("action", map[string]any{"text": res.action}, true)
	}
	if res.image != nil {
		a.looking = true
		go a.describe(res.image)
	}
	return res.launched
}

// describe sends the agent a description of a screenshot, since it can only
// take text.
func (a *Assistant) describe(img []byte) {
	ctx, cancel := context.WithTimeout(a.runCtx, 30*time.Second)
	defer cancel()
	msg, err := a.xai.Chat(ctx, a.cfg.ChatModel, []xai.Message{{Role: "user", Content: describePrompt, Images: [][]byte{img}}}, nil, nil)
	if err != nil {
		log.Printf("grokbot: describe screen: %v", err)
		a.askBot("[[tv look]] failed: the screenshot couldn't be described.")
		return
	}
	a.askBot("[[tv look]] The TV screen right now: " + strings.TrimSpace(msg.Content))
}
