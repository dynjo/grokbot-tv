package assistant

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"grokbot/internal/botapi"
	"grokbot/internal/bridge"
	"grokbot/internal/remote"
	"grokbot/internal/speaker"
	"grokbot/internal/xai"
)

// The remote mic is read in 80 ms chunks.
const chunkDur = 80 * time.Millisecond

var defaultPrompt = `You are Grok, the voice assistant built into this LG TV. The user holds the remote's Voice button and speaks; your reply is shown on screen and read aloud.
Keep replies short: one to three plain sentences unless the user asks for more. No markdown, lists, emoji or links.
Use the tools to open apps, switch inputs and change the volume, but only when the user explicitly asks for that action. Never open an app or change anything on your own initiative, for example to go with a joke or an answer. After a tool call, confirm in a few words.
This is a conversation: after you answer, the user can simply keep talking.
You can see the TV screen with look_at_screen. Use it whenever the user asks about what is on screen instead of saying you can't see it.
Start every reply with your mood in square brackets, which sets the expression on your animated face: one of ` + moodList + `. For example: [curious] Good question!
The current local time is %s.`

// screenshotNote introduces a look_at_screen image. Without it the model
// looks for a TV in the picture instead of treating it as the screen.
const screenshotNote = "This image is a screenshot: exactly what the TV is displaying right now (the picture itself, not a photo of a TV). Answer the user's question about it."

// moods are the morph-bot face states Grok may pick for a reply.
var moods = []string{"happy", "excited", "laughing", "playful", "curious", "proud", "shy", "surprised", "confused", "suspicious", "sad", "bored", "drowsy", "scared", "angry"}

var moodList = "[" + strings.Join(moods, "], [") + "]"

// Assistant runs one voice session at a time.
type Assistant struct {
	cfg    Config
	hub    *bridge.Hub
	xai    *xai.Client
	tools  []xai.Tool
	trig   chan string
	inbox  chan botMessage // from the user's Grok Bot agent
	runCtx context.Context

	mu          sync.Mutex
	gen         int                // session counter
	cancel      context.CancelFunc // cancels the running session
	interrupt   context.CancelFunc // cancels the current turn's answer
	finish      chan struct{}      // closed to end recording early
	returnTo    string             // app on screen before the overlay opened
	history     []xai.Message
	lastTurn    time.Time
	restoreMute bool
	voiceGate   float64 // follow-up speech gate from the last conversation
	bot         *botapi.Client
	botAuth     botAuth
	live        chan botMessage // the agent's messages, while a conversation is on screen
	looking     bool            // the agent asked for [[tv look]] and awaits the description
	introduced  bool            // this conversation's first message has gone to the agent
}

func (a *Assistant) setInterrupt(f context.CancelFunc) {
	a.mu.Lock()
	a.interrupt = f
	a.mu.Unlock()
}

// New returns an assistant. hub receives overlay events and messages.
func New(cfg Config, hub *bridge.Hub) *Assistant {
	a := &Assistant{cfg: cfg, hub: hub, xai: xai.New(cfg.XAIAPIKey), tools: tvTools,
		trig: make(chan string, 1), inbox: make(chan botMessage, 16), runCtx: context.Background()}
	hub.OnMessage = a.onOverlay
	return a
}

// Trigger acts like a Voice button press.
func (a *Assistant) Trigger(reason string) {
	select {
	case a.trig <- reason:
	default:
	}
}

// Busy reports whether a session is running.
func (a *Assistant) Busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancel != nil
}

func (a *Assistant) onOverlay(msg map[string]any) {
	switch msg["type"] {
	case "cancel":
		a.mu.Lock()
		if a.cancel != nil {
			a.cancel()
		}
		a.mu.Unlock()
	case "listen":
		a.Trigger("overlay")
	case "log":
		log.Printf("overlay: %v", msg["msg"])
	}
}

// Run starts a conversation when the Voice button is pressed, until ctx is
// done. During a conversation a press ends the recording early, or
// interrupts Grok's answer and listens again. Messages from the user's Grok
// Bot agent start a conversation that opens with them, once the TV is free.
func (a *Assistant) Run(ctx context.Context, presses <-chan time.Time) {
	a.mu.Lock()
	a.runCtx = ctx
	a.mu.Unlock()
	var (
		done    chan struct{}
		pending []botMessage
	)
	start := func(reason string, msgs []botMessage) {
		log.Printf("session: started by %s", reason)
		done = make(chan struct{})
		go a.session(ctx, done, msgs)
	}
	press := func(reason string) {
		if done == nil {
			start(reason, nil)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case a.finish != nil:
			close(a.finish)
			a.finish = nil
		case a.interrupt != nil:
			log.Printf("session: answer interrupted by %s", reason)
			a.interrupt()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			done = nil
			if len(pending) > 0 {
				start("message from "+pending[0].from, pending)
				pending = nil
			}
		case m := <-a.inbox:
			if done == nil {
				start("message from "+m.from, []botMessage{m})
			} else {
				pending = append(pending, m)
			}
		case <-presses:
			press("voice key")
		case reason := <-a.trig:
			press(reason)
		}
	}
}

// session runs one conversation: it keeps the remote mic on and alternates
// listening and answering until the user stops talking or presses Back.
// With msgs, it first reads them out and then listens for a reply.
func (a *Assistant) session(parent context.Context, done chan<- struct{}, msgs []botMessage) {
	ctx, cancel := context.WithCancel(parent)
	a.mu.Lock()
	a.gen++
	a.cancel = cancel
	a.returnTo = ""
	a.introduced = false
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.cancel = nil
		a.interrupt = nil
		a.finish = nil
		a.mu.Unlock()
		close(done)
	}()

	var live <-chan botMessage
	if a.botEnabled() {
		live = a.openLive()
		defer a.closeLive()
		go a.warmUp()
	}
	a.hub.ClearReplay()
	if len(msgs) == 0 {
		a.hub.Send("listening", nil, true)
	}
	go func() {
		if fg := bridge.ForegroundApp(parent); fg != a.cfg.OverlayApp {
			a.mu.Lock()
			a.returnTo = fg
			a.mu.Unlock()
		}
		if err := bridge.Launch(parent, a.cfg.OverlayApp, map[string]any{"source": "grokbot"}); err != nil {
			log.Printf("session: launch overlay: %v", err)
		}
	}()
	first, thresh := 0, a.cfg.MinSpeechRMS
	if len(msgs) > 0 {
		ictx, stop := context.WithCancel(ctx)
		a.setInterrupt(stop)
		launched := a.sayAll(ictx, msgs, live)
		a.setInterrupt(nil)
		stop()
		if ctx.Err() != nil {
			a.close(parent, 0)
			return
		}
		if launched {
			a.close(ctx, 1500*time.Millisecond)
			return
		}
		// Go straight to listening for an answer, at the loudness the user
		// spoke at last time. Until then, be strict: the mic also hears the
		// TV.
		first = 1
		a.mu.Lock()
		thresh = a.voiceGate
		a.mu.Unlock()
		if thresh == 0 {
			thresh = 3 * a.cfg.MinSpeechRMS
		}
	}
	t0 := time.Now()
	mic := &liveMic{}
	err := mic.open(ctx)
	if err != nil {
		log.Printf("session: %v", err)
		if ctx.Err() != nil {
			a.close(parent, 0)
			return
		}
		if first > 0 { // the remote is asleep: no reply then
			a.close(ctx, time.Duration(a.cfg.LingerSec*float64(time.Second)))
			return
		}
		a.hub.Send("error", map[string]any{"message": "I couldn't turn on the remote's microphone."}, true)
		a.close(ctx, 3*time.Second)
		return
	}
	defer mic.Close()
	log.Printf("session: mic on after %v", time.Since(t0).Round(time.Millisecond))

	followUp := time.Duration(a.cfg.FollowUpSec * float64(time.Second))
	for turn := first; ; turn++ {
		wait := time.Duration(a.cfg.NoSpeechSec * float64(time.Second))
		if turn > 0 {
			if followUp <= 0 {
				a.close(ctx, time.Duration(a.cfg.LingerSec*float64(time.Second)))
				return
			}
			// Keep the answer on screen and listen for a follow-up.
			mic.Flush()
			a.hub.Send("followUp", nil, false)
			wait = followUp
		}
		pcm := a.record(ctx, mic, wait, thresh, turn > 0)
		if ctx.Err() != nil {
			a.close(parent, 0)
			return
		}
		if pcm == nil && turn > 0 && len(live) > 0 {
			// The agent said more while the user was quiet.
			ictx, stop := context.WithCancel(ctx)
			a.setInterrupt(stop)
			launched := a.sayAll(ictx, nil, live)
			a.setInterrupt(nil)
			stop()
			if ctx.Err() == nil && !launched {
				continue
			}
			a.close(ctx, 1500*time.Millisecond)
			return
		}
		if pcm == nil {
			if turn == 0 {
				a.hub.Send("error", map[string]any{"message": "I didn't hear anything."}, true)
				a.close(ctx, 2*time.Second)
			} else {
				a.close(ctx, 0)
			}
			return
		}
		// Without a press the mic also hears the TV, so follow-ups must be
		// about as loud as the user's own voice was.
		if turn == 0 {
			thresh = max(a.cfg.MinSpeechRMS, voiceLevel(pcm, a.cfg.MinSpeechRMS)/4)
			log.Printf("session: follow-up speech gate %.0f", thresh)
			a.mu.Lock()
			a.voiceGate = thresh
			a.mu.Unlock()
		}
		if !a.turn(ctx, parent, pcm, turn > 0, live) {
			return
		}
	}
}

// record listens for one request, muting the TV meanwhile. On a follow-up
// turn the screen is only cleared once the user starts speaking.
func (a *Assistant) record(ctx context.Context, mic *liveMic, wait time.Duration, thresh float64, followUp bool) []int16 {
	finish := make(chan struct{})
	a.mu.Lock()
	a.finish = finish
	a.mu.Unlock()
	if a.cfg.MuteWhileListening {
		if _, muted, err := bridge.Volume(ctx); err == nil && !muted {
			if bridge.SetMuted(ctx, true) == nil {
				a.restoreMute = true
			}
		}
	}
	onSpeech := func() {
		if followUp {
			a.hub.ClearReplay()
			a.hub.Send("listening", nil, true)
		}
	}
	pcm := a.listen(ctx, mic, finish, wait, thresh, onSpeech)
	a.mu.Lock()
	a.finish = nil
	a.mu.Unlock()
	if a.restoreMute {
		bridge.SetMuted(context.Background(), false)
		a.restoreMute = false
	}
	return pcm
}

// turn transcribes pcm, answers it and speaks the answer. It returns false
// when the conversation is over (an error, or Grok opened an app). A press
// during the answer cuts it short and the conversation carries on.
func (a *Assistant) turn(ctx, parent context.Context, pcm []int16, followUp bool, live <-chan botMessage) bool {
	tctx, interrupt := context.WithCancel(ctx)
	defer interrupt()
	a.mu.Lock()
	a.interrupt = interrupt
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.interrupt = nil
		a.mu.Unlock()
	}()
	interrupted := func() bool {
		if ctx.Err() == nil && tctx.Err() != nil {
			a.hub.ClearReplay()
			a.hub.Send("listening", nil, true)
			return true
		}
		return false
	}

	a.hub.Send("transcribing", nil, true)
	t0 := time.Now()
	text, err := a.xai.Transcribe(tctx, a.cfg.STTModel, a.cfg.Language, pcm)
	if interrupted() {
		return true
	}
	if err != nil {
		log.Printf("session: stt: %v", err)
		a.fail(ctx, parent, "Speech recognition failed.", err)
		return false
	}
	log.Printf("session: heard %q (%.1fs audio, stt %v)", text, float64(len(pcm))/remote.Rate, time.Since(t0).Round(time.Millisecond))
	if text == "" {
		if followUp { // background noise, not the user: just finish
			a.close(ctx, 0)
			return false
		}
		a.hub.Send("error", map[string]any{"message": "Sorry, I didn't catch that."}, true)
		a.close(ctx, 2500*time.Millisecond)
		return false
	}
	a.hub.Send("transcript", map[string]any{"text": text}, true)
	if a.botEnabled() {
		intro := !a.introduced
		a.introduced = true // the user is talking: [[tv look]] is allowed
		more := a.handOver(ctx, tctx, text, intro, live)
		interrupted()
		return more
	}

	answer, launched, err := a.answer(tctx, text)
	if interrupted() {
		return true
	}
	if err != nil {
		log.Printf("session: chat: %v", err)
		a.fail(ctx, parent, "Grok is unavailable right now.", err)
		return false
	}
	a.hub.Send("answerDone", nil, true)
	log.Printf("session: answered %q", answer)
	if launched {
		a.close(ctx, 1500*time.Millisecond)
		return false
	}
	if a.cfg.TTS && strings.TrimSpace(answer) != "" {
		a.speak(tctx, answer)
	}
	interrupted()
	return ctx.Err() == nil
}

// listen records the user's request from the remote mic: up to wait for
// speech to start, then until EndSilenceSec of quiet, MaxListenSec in total,
// or finish is closed (the button was pressed again). Speech is anything
// louder than thresh. onSpeech is called when speech starts. It returns nil
// if no speech was heard.
func (a *Assistant) listen(ctx context.Context, mic *liveMic, finish <-chan struct{}, wait time.Duration, thresh float64, onSpeech func()) []int16 {
	var (
		pcm     []int16
		elapsed time.Duration
		silence time.Duration
		voiced  int
		heard   bool
	)
	result := func() []int16 {
		if heard {
			return pcm
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-finish:
			log.Printf("session: recording ended by button")
			return result()
		default:
		}
		chunk, err := mic.read(ctx)
		if err != nil {
			log.Printf("session: mic: %v", err)
			return result()
		}
		elapsed += chunkDur
		rms := remote.RMS(chunk)
		if !heard {
			// Keep a little audio from before speech was detected.
			pcm = append(pcm, chunk...)
			if keep := 6 * len(chunk); len(pcm) > keep {
				pcm = pcm[len(pcm)-keep:]
			}
		} else {
			pcm = append(pcm, chunk...)
		}
		a.hub.Send("level", map[string]any{"level": min(1, rms/(thresh*6))}, false)
		if rms > thresh {
			voiced++
			silence = 0
			if voiced >= 3 && !heard {
				heard = true
				onSpeech()
			}
		} else {
			voiced = 0
			silence += chunkDur
		}
		switch {
		case heard && silence >= time.Duration(a.cfg.EndSilenceSec*float64(time.Second)):
			return pcm
		case elapsed >= time.Duration(a.cfg.MaxListenSec*float64(time.Second))+wait:
			return result()
		case !heard && elapsed >= wait:
			return nil
		}
	}
}

// voiceLevel is the median loudness of the speech in pcm: the 80 ms chunks
// louder than gate.
func voiceLevel(pcm []int16, gate float64) float64 {
	var levels []float64
	for i := 0; i+remote.ChunkSamples <= len(pcm); i += remote.ChunkSamples {
		if l := remote.RMS(pcm[i : i+remote.ChunkSamples]); l > gate {
			levels = append(levels, l)
		}
	}
	if len(levels) == 0 {
		return 0
	}
	sort.Float64s(levels)
	return levels[len(levels)/2]
}

// liveMic is the remote mic for a conversation. The remote ends its stream
// after about 15 seconds, so liveMic opens it again when that happens.
// The mic also makes a pop as it powers up, so the first ~300 ms after
// each open are dropped.
type liveMic struct {
	m    *remote.Mic
	skip int // chunks still to drop
}

func (l *liveMic) open(ctx context.Context) error {
	m, err := remote.OpenMic(ctx, 3*time.Second)
	l.m, l.skip = m, 4
	return err
}

func (l *liveMic) read(ctx context.Context) ([]int16, error) {
	for {
		chunk, err := l.next(ctx)
		if err != nil || l.skip == 0 {
			return chunk, err
		}
		l.skip--
	}
}

func (l *liveMic) next(ctx context.Context) ([]int16, error) {
	chunk, err := l.m.Read(2 * time.Second)
	if err != remote.ErrEnded {
		return chunk, err
	}
	l.m.Close()
	t0 := time.Now()
	if err := l.open(ctx); err != nil {
		return nil, err
	}
	log.Printf("session: mic reopened after %v", time.Since(t0).Round(time.Millisecond))
	return l.m.Read(2 * time.Second)
}

// Flush discards queued audio.
func (l *liveMic) Flush() { l.m.Flush() }

// Close turns the mic off.
func (l *liveMic) Close() {
	if l.m != nil {
		l.m.Close()
	}
}

// answer asks Grok, running TV tool calls until it gives a final reply.
func (a *Assistant) answer(ctx context.Context, text string) (string, bool, error) {
	prompt := a.cfg.SystemPrompt
	if prompt == "" {
		prompt = fmt.Sprintf(defaultPrompt, time.Now().Format("Monday 2 January 2006, 15:04"))
	}
	msgs := []xai.Message{{Role: "system", Content: prompt}}
	var prevReply string // Grok's last reply, which may have offered an action
	a.mu.Lock()
	if time.Since(a.lastTurn) < time.Duration(a.cfg.HistoryMinutes*float64(time.Minute)) {
		msgs = append(msgs, a.history...)
		if n := len(a.history); n > 0 {
			prevReply = a.history[n-1].Content
		}
	} else {
		a.history = nil
	}
	a.mu.Unlock()
	user := xai.Message{Role: "user", Content: text}
	msgs = append(msgs, user)

	launched := false
	done := map[string]bool{} // tool calls already run for this request
	for round := 0; round < 4; round++ {
		var images [][]byte
		a.hub.Send("thinking", nil, true)
		var tag moodTag
		msg, err := a.xai.Chat(ctx, a.cfg.ChatModel, msgs, a.tools, func(delta string) {
			mood, text := tag.feed(delta)
			if mood != "" {
				a.hub.Send("mood", map[string]any{"mood": mood}, true)
			}
			if text != "" {
				a.hub.Send("answerDelta", map[string]any{"text": text}, true)
			}
		})
		if err != nil {
			return "", false, err
		}
		msg.Content = stripMood(msg.Content)
		msgs = append(msgs, msg)
		if len(msg.ToolCalls) == 0 {
			a.mu.Lock()
			a.history = append(a.history, user, xai.Message{Role: "assistant", Content: msg.Content})
			if n := len(a.history); n > 8 {
				a.history = a.history[n-8:]
			}
			a.lastTurn = time.Now()
			a.mu.Unlock()
			return msg.Content, launched, nil
		}
		for _, call := range msg.ToolCalls {
			var res toolResult
			key := call.Function.Name + call.Function.Arguments
			if why := unasked(call, text, prevReply); why != "" {
				log.Printf("session: refused %s(%s): %s", call.Function.Name, call.Function.Arguments, why)
				res = toolResult{text: "Not done: " + why + " Only use tools when the user asks for that action. Just answer instead."}
			} else if done[key] {
				// Models sometimes repeat a call in the next round, which
				// would turn the volume up twice.
				res = toolResult{text: "Already done; don't repeat it."}
			} else {
				done[key] = true
				res = a.runTool(ctx, call)
			}
			log.Printf("session: tool %s(%s) -> %s", call.Function.Name, call.Function.Arguments, res.text)
			if res.action != "" {
				a.hub.Send("action", map[string]any{"text": res.action}, true)
			}
			if res.launched {
				launched = true
				a.mu.Lock()
				a.returnTo = "" // stay on the app Grok opened
				a.mu.Unlock()
			}
			msgs = append(msgs, xai.Message{Role: "tool", ToolCallID: call.ID, Content: res.text})
			if res.image != nil {
				images = append(images, res.image)
			}
		}
		// Tool results can only be text; screenshots follow as a user
		// message.
		if len(images) > 0 {
			msgs = append(msgs, xai.Message{Role: "user", Content: screenshotNote, Images: images})
		}
	}
	return "", launched, fmt.Errorf("too many tool rounds")
}

// speak reads the answer aloud on the TV and waits for it to finish. The
// overlay gets the voice's loudness to animate the face.
func (a *Assistant) speak(ctx context.Context, text string) {
	mp3, err := a.xai.Speak(ctx, a.cfg.TTSVoice, a.cfg.Language, speakable(text))
	if err != nil {
		log.Printf("session: tts: %v", err)
		return
	}
	a.hub.Send("speaking", nil, true)
	err = speaker.Play(ctx, mp3, func(level float64) {
		a.hub.Send("voiceLevel", map[string]any{"level": level}, false)
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("session: play: %v", err)
	}
	a.hub.Send("spoken", nil, true)
}

func (a *Assistant) fail(ctx, parent context.Context, msg string, err error) {
	if ctx.Err() != nil {
		a.close(parent, 0)
		return
	}
	if e, ok := err.(*xai.Error); ok && (e.Status == 401 || e.Status == 403) {
		msg = "The xAI API key was rejected. Check xai_api_key in config.json."
	}
	a.hub.Send("error", map[string]any{"message": msg}, true)
	a.close(ctx, 4*time.Second)
}

// close waits (unless the session is cancelled), then dismisses the overlay.
func (a *Assistant) close(ctx context.Context, after time.Duration) {
	select {
	case <-time.After(after):
	case <-ctx.Done():
	}
	a.hub.Send("close", nil, false)
	a.hub.ClearReplay()
	a.mu.Lock()
	gen, returnTo := a.gen, a.returnTo
	a.mu.Unlock()
	// The overlay closes itself; make sure it is gone if it didn't connect,
	// unless a new session has opened it again. If webOS went Home instead
	// of back to the app that was playing, switch back to it.
	time.AfterFunc(1500*time.Millisecond, func() {
		bg := context.Background()
		a.mu.Lock()
		stale := a.gen != gen
		a.mu.Unlock()
		if stale {
			return
		}
		fg := bridge.ForegroundApp(bg)
		if fg == a.cfg.OverlayApp {
			bridge.Close(bg, a.cfg.OverlayApp)
			fg = ""
		}
		if returnTo != "" && fg != returnTo {
			log.Printf("session: returning to %s (foreground was %q)", returnTo, fg)
			if err := bridge.Launch(bg, returnTo, nil); err != nil {
				log.Printf("session: return to %s: %v", returnTo, err)
			}
		}
	})
}

var markdown = regexp.MustCompile("[*_#`>]+")

func speakable(text string) string {
	return strings.TrimSpace(markdown.ReplaceAllString(text, ""))
}

var moodPrefix = regexp.MustCompile(`^\s*\[([A-Za-z -]{1,20})\]\s*`)

// stripMood removes a leading [mood] tag.
func stripMood(text string) string {
	return moodPrefix.ReplaceAllString(text, "")
}

// moodTag splits a streamed reply into its leading [mood] tag and the text.
type moodTag struct {
	buf  string
	done bool
	trim bool // drop spaces between the tag and the text
}

// feed takes the next streamed delta. It returns the mood once the tag is
// complete (only if it is a known mood) and the text to show, holding text
// back while it might still be part of the tag.
func (t *moodTag) feed(delta string) (mood, text string) {
	if t.done {
		if t.trim {
			delta = strings.TrimLeft(delta, " ")
			t.trim = delta == ""
		}
		return "", delta
	}
	t.buf += delta
	trimmed := strings.TrimLeft(t.buf, " \n")
	switch {
	case trimmed == "":
		return "", ""
	case trimmed[0] != '[':
		t.done = true
		return "", t.buf
	}
	end := strings.IndexByte(trimmed, ']')
	if end < 0 {
		if len(trimmed) > 24 { // not a tag after all
			t.done = true
			return "", t.buf
		}
		return "", ""
	}
	t.done = true
	name := strings.ToLower(strings.TrimSpace(trimmed[1:end]))
	for _, m := range moods {
		if m == name {
			mood = m
		}
	}
	text = strings.TrimLeft(trimmed[end+1:], " ")
	t.trim = text == ""
	return mood, text
}
