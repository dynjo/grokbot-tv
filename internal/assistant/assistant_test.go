package assistant

import (
	"encoding/json"
	"testing"

	"github.com/dynjo/grokbot-tv/internal/bridge"
	"github.com/dynjo/grokbot-tv/internal/xai"
)

func TestMatchApp(t *testing.T) {
	apps := []bridge.App{{ID: "netflix", Title: "Netflix"}, {ID: "prime", Title: "Prime Video"}, {ID: "yt", Title: "YouTube"}, {ID: "atv", Title: "Apple TV"}, {ID: "dp", Title: "Disney+"}}
	for _, tc := range []struct{ name, want string }{
		{"netflix", "netflix"},
		{"Prime", "prime"},
		{"amazon prime video", "prime"},
		{"you tube", "yt"},
		{"Disney Plus", "dp"},
		{"disney", "dp"},
		{"spotify", ""},
	} {
		got, ok := matchApp(apps, tc.name)
		if (tc.want == "") == ok || (ok && got.ID != tc.want) {
			t.Errorf("matchApp(%q) = %q %v, want %q", tc.name, got.ID, ok, tc.want)
		}
	}
}

func TestMoodTag(t *testing.T) {
	for _, tc := range []struct {
		deltas     []string
		mood, text string
	}{
		{[]string{"[cur", "ious] Good", " question!"}, "curious", "Good question!"},
		{[]string{" [Happy]", " Sure."}, "happy", "Sure."},
		{[]string{"[dancing] Hi"}, "", "Hi"},
		{[]string{"No tag here"}, "", "No tag here"},
		{[]string{"[", "this is not a mood tag at all", "]"}, "", "[this is not a mood tag at all]"},
	} {
		var tag moodTag
		var mood, text string
		for _, d := range tc.deltas {
			m, x := tag.feed(d)
			if m != "" {
				mood = m
			}
			text += x
		}
		if mood != tc.mood || text != tc.text {
			t.Errorf("%q: got mood %q text %q, want %q %q", tc.deltas, mood, text, tc.mood, tc.text)
		}
	}
	if got := stripMood("[proud] Done."); got != "Done." {
		t.Errorf("stripMood = %q", got)
	}
}

func TestUnasked(t *testing.T) {
	call := func(name, args string) xai.ToolCall {
		var c xai.ToolCall
		c.Function.Name, c.Function.Arguments = name, args
		return c
	}
	for _, tc := range []struct {
		call       xai.ToolCall
		said, prev string
		ok         bool
	}{
		{call("open_app", `{"name":"Netflix"}`), "Tell me a joke.", "", false},
		{call("open_app", `{"name":"Netflix"}`), "Open Netflix", "", true},
		{call("open_app", `{"name":"Netflix"}`), "Netflix please", "", true},
		{call("open_app", `{"name":"Prime Video"}`), "I want to watch something on Prime", "", true},
		{call("open_app", `{"name":"Disney+"}`), "Disney plus", "", true},
		{call("open_app", `{"name":"Netflix"}`), "Yes please", "Want me to open Netflix?", true},
		{call("open_app", `{"name":"Netflix"}`), "Yes please", "Want me to tell another joke?", false},
		{call("switch_input", `{"input":"hdmi2"}`), "Switch to the Xbox", "", true},
		{call("switch_input", `{"input":"hdmi2"}`), "What's the weather?", "", false},
		{call("set_mute", `{"muted":true}`), "Hey Grok, you there?", "", false},
		{call("set_mute", `{"muted":true}`), "Mute the TV", "", true},
		{call("change_volume", `{"delta":5}`), "Turn it up a bit", "", true},
	} {
		why := unasked(tc.call, tc.said, tc.prev)
		if (why == "") != tc.ok {
			t.Errorf("%s(%s) after %q: refused=%q, want ok=%v", tc.call.Function.Name, tc.call.Function.Arguments, tc.said, why, tc.ok)
		}
	}
}

func TestTVCommands(t *testing.T) {
	text, cmds := tvCommands("Turning it down.\n[[tv volume -5]]\n\n\n[[TV open Prime Video]] Enjoy!")
	if text != "Turning it down.\n\nEnjoy!" {
		t.Errorf("text = %q", text)
	}
	if len(cmds) != 2 || cmds[0] != "volume -5" || cmds[1] != "open Prime Video" {
		t.Fatalf("cmds = %q", cmds)
	}
	for _, c := range []struct {
		cmd, name, args string
	}{
		{"volume -5", "change_volume", `{"delta":-5}`},
		{"volume +3", "change_volume", `{"delta":3}`},
		{"volume 30", "set_volume", `{"level":30}`},
		{"open Prime Video", "open_app", `{"name":"Prime Video"}`},
		{"input HDMI 2", "switch_input", `{"input":"hdmi2"}`},
		{"mute", "set_mute", `{"muted":true}`},
		{"look", "look_at_screen", `null`},
	} {
		name, args, ok := commandCall(c.cmd)
		buf, _ := json.Marshal(args)
		if !ok || name != c.name || string(buf) != c.args {
			t.Errorf("%q -> %s %s %v", c.cmd, name, buf, ok)
		}
	}
	if _, _, ok := commandCall("volume loud"); ok {
		t.Error("volume loud accepted")
	}
}
