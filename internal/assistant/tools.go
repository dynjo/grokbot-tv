package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"grokbot/internal/bridge"
	"grokbot/internal/xai"
)

func tool(name, desc string, props map[string]any, required ...string) xai.Tool {
	if required == nil {
		required = []string{}
	}
	return xai.Tool{Type: "function", Function: xai.ToolFunction{
		Name:        name,
		Description: desc,
		Parameters:  map[string]any{"type": "object", "properties": props, "required": required},
	}}
}

var tvTools = []xai.Tool{
	tool("open_app", "Open an installed TV app, e.g. Netflix, YouTube, Prime Video, Settings. Only when the user asks to open or watch it.",
		map[string]any{"name": map[string]any{"type": "string", "description": "App name"}}, "name"),
	tool("switch_input", "Switch the TV to an input: hdmi1, hdmi2, hdmi3, hdmi4 or tv (live TV).",
		map[string]any{"input": map[string]any{"type": "string", "enum": []string{"hdmi1", "hdmi2", "hdmi3", "hdmi4", "tv"}}}, "input"),
	tool("set_volume", "Set the TV volume to an absolute level.",
		map[string]any{"level": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}}, "level"),
	tool("change_volume", "Raise (positive) or lower (negative) the TV volume by some steps.",
		map[string]any{"delta": map[string]any{"type": "integer"}}, "delta"),
	tool("set_mute", "Mute or unmute the TV.",
		map[string]any{"muted": map[string]any{"type": "boolean"}}, "muted"),
	tool("look_at_screen", "Take a screenshot of what is on the TV screen right now and look at it. Use it whenever the user asks about what is on screen: who someone is, what they are watching, what something says.",
		map[string]any{}),
}

// toolResult is what a tool reports back to the model and the overlay.
type toolResult struct {
	text     string // for the model
	action   string // short caption for the overlay, "" if the call failed
	launched bool   // an app now covers the overlay
	image    []byte // a screenshot for the model to look at
}

func (a *Assistant) runTool(ctx context.Context, call xai.ToolCall) toolResult {
	var args struct {
		Name  string `json:"name"`
		Input string `json:"input"`
		Level *int   `json:"level"`
		Delta int    `json:"delta"`
		Muted bool   `json:"muted"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return toolResult{text: "bad arguments: " + err.Error()}
	}
	fail := func(err error) toolResult { return toolResult{text: "failed: " + err.Error()} }
	switch call.Function.Name {
	case "open_app":
		apps, err := bridge.Apps(ctx)
		if err != nil {
			return fail(err)
		}
		app, ok := matchApp(apps, args.Name)
		if !ok {
			titles := make([]string, 0, len(apps))
			for _, a := range apps {
				titles = append(titles, a.Title)
			}
			return toolResult{text: fmt.Sprintf("No app matches %q. Installed apps: %s", args.Name, strings.Join(titles, ", "))}
		}
		if err := bridge.Launch(ctx, app.ID, nil); err != nil {
			return fail(err)
		}
		return toolResult{text: "Opened " + app.Title, action: "Opening " + app.Title, launched: true}
	case "switch_input":
		id, label := "com.webos.app.livetv", "Live TV"
		if in := strings.ToLower(strings.ReplaceAll(args.Input, " ", "")); strings.HasPrefix(in, "hdmi") {
			id, label = "com.webos.app."+in, strings.ToUpper(in[:4])+" "+in[4:]
		}
		if err := bridge.Launch(ctx, id, nil); err != nil {
			return fail(err)
		}
		return toolResult{text: "Switched to " + label, action: "Switching to " + label, launched: true}
	case "set_volume", "change_volume":
		level := 0
		if args.Level != nil {
			level = *args.Level
		}
		if call.Function.Name == "change_volume" {
			cur, _, err := bridge.Volume(ctx)
			if err != nil {
				return fail(err)
			}
			level = cur + args.Delta
		}
		level = max(0, min(100, level))
		if err := bridge.SetVolume(ctx, level); err != nil {
			return fail(err)
		}
		a.restoreMute = false // the user asked for sound changes; leave them be
		return toolResult{text: fmt.Sprintf("Volume is now %d", level), action: fmt.Sprintf("Volume %d", level)}
	case "look_at_screen":
		// Hide the overlay so it isn't in the picture.
		a.hub.Send("peek", map[string]any{"hidden": true}, false)
		time.Sleep(250 * time.Millisecond)
		img, err := bridge.Screenshot(ctx, 1280, 720)
		a.hub.Send("peek", map[string]any{"hidden": false}, false)
		if err != nil {
			return fail(err)
		}
		return toolResult{
			text:   "Screenshot taken; it follows as an image. If it is black, the content is probably copy-protected and can't be captured.",
			action: "Looking at the screen",
			image:  img,
		}
	case "set_mute":
		if err := bridge.SetMuted(ctx, args.Muted); err != nil {
			return fail(err)
		}
		a.restoreMute = false
		if args.Muted {
			return toolResult{text: "Muted", action: "Muted"}
		}
		return toolResult{text: "Unmuted", action: "Unmuted"}
	}
	return toolResult{text: "unknown tool " + call.Function.Name}
}

func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// matchApp finds the app whose title best matches name: exact, then prefix,
// then substring either way.
func matchApp(apps []bridge.App, name string) (bridge.App, bool) {
	want := normalize(name)
	if want == "" {
		return bridge.App{}, false
	}
	for _, pass := range []func(t string) bool{
		func(t string) bool { return t == want },
		func(t string) bool { return strings.HasPrefix(t, want) },
		func(t string) bool { return strings.Contains(t, want) || strings.Contains(want, t) },
	} {
		for _, app := range apps {
			if t := normalize(app.Title); t != "" && pass(t) {
				return app, true
			}
		}
	}
	return bridge.App{}, false
}

// Words that show the user asked for a TV action. Models sometimes call
// tools unprompted (opening Netflix after telling a joke), so each call is
// checked against what the user actually said.
var (
	launchWords = wordSet("open launch start play watch put go switch take bring")
	inputWords  = wordSet("hdmi hdmi1 hdmi2 hdmi3 hdmi4 input source switch tv xbox playstation ps5 ps4 console cable antenna channel")
	volumeWords = wordSet("volume louder quieter loud quiet softer mute unmute sound silence turn up down")
	yesWords    = wordSet("yes yeah yep sure ok okay please go")
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func hasAny(text string, set map[string]bool) bool {
	for _, w := range words(text) {
		if set[w] {
			return true
		}
	}
	return false
}

// unasked returns why a tool call doesn't match the user's request, or ""
// if it does. said is what the user just said; prevReply is Grok's previous
// reply, so "yes please" can accept an offer such as "Want me to open
// Netflix?".
func unasked(call xai.ToolCall, said, prevReply string) string {
	switch call.Function.Name {
	case "open_app":
		var args struct{ Name string }
		json.Unmarshal([]byte(call.Function.Arguments), &args)
		name := normalize(args.Name)
		if name == "" {
			return ""
		}
		if strings.Contains(normalize(said), name) || hasAny(said, launchWords) {
			return ""
		}
		for _, w := range words(args.Name) {
			if len(w) >= 4 && hasAny(said, wordSet(w)) {
				return ""
			}
		}
		if strings.Contains(normalize(prevReply), name) && hasAny(said, yesWords) {
			return ""
		}
		return "the user didn't ask to open " + args.Name + "."
	case "switch_input":
		if hasAny(said, inputWords) || hasAny(said, launchWords) || strings.Contains(normalize(said), "hdmi") {
			return ""
		}
		return "the user didn't ask to change the input."
	case "set_volume", "change_volume", "set_mute":
		if hasAny(said, volumeWords) {
			return ""
		}
		return "the user didn't ask to change the sound."
	}
	return ""
}
