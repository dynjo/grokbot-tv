# grokbot-tv

A push-to-talk Grok assistant for rooted LG webOS TVs. Hold the Magic Remote's Voice button and ask something. An overlay with an animated face slides up over whatever is playing. Grok (xAI) then transcribes the request, answers it or acts on the TV, and reads the answer aloud.

It replaces LG's own voice assistant. While grokbot runs, the Voice button only opens grokbot.

> An unofficial hobby project, not affiliated with or endorsed by xAI, LG or Cursor. "Grok" and "Grok Bot" are trademarks of xAI, and "LG", "webOS" and "Magic Remote" are trademarks of LG Electronics. Rooting your TV may void its warranty. The optional Grok Bot integration uses undocumented endpoints and the app's own sign-in, which may be against its terms of service. Use at your own risk.

```
Voice button ─ syslog (lginput2 KEY_VOICE) ─▶ grokbotd ─┬─ voiceinput/startStreaming ─▶ remote mic PCM (unix socket)
                                                         ├─ xAI /v1/stt ─▶ /v1/chat/completions (+ TV tools) ─▶ /v1/tts
                                                         └─ ws://127.0.0.1:8679 ─▶ overlay app (com.grokbot.overlay)
```

## Pieces

| Path | What |
|---|---|
| `cmd/grokbotd` | The daemon: a static linux/arm64 Go binary with no runtime dependencies. The TV's userland is 32-bit and has no Python, but its kernel runs 64-bit binaries. |
| `internal/remote` | Voice button detection, the remote mic stream, and keeping LG's voice assistant out of the way. |
| `internal/assistant` | The session: overlay, mute, endpointing, STT, chat with TV tools and moods, TTS. |
| `internal/bridge` | WebSocket server for the overlay and `luna-send` helpers. |
| `internal/xai` | xAI API client (STT, streaming chat with tool calls, TTS). |
| `internal/speaker` | Plays the spoken reply on the TV's `ptts` audio channel. |
| `internal/botapi` | Talks to your own Grok Bot agent (see below). |
| `scripts/sync-grok-bot.sh` | Copies the Grok Bot app's sign-in from a Mac to the TV. |
| `overlay/` | The webOS web app that draws the bottom sheet and the face, and plays the reply. |
| `scripts/build-face.sh` | Bundles the face (`<morph-bot>`) from [grokbot-animation](https://github.com/iduu/grokbot-animation) for the TV's browser. |

TV tools available to Grok: open an app by name, switch input (HDMI 1–4, live TV), set or change the volume, mute or unmute, and look at the screen. For a screenshot, the overlay hides for a moment and the image goes to Grok with the question. Copy-protected video comes out black.

Grok tends to be over-eager with tools, for example opening Netflix after telling a joke. So each call is checked against what the user said (`unasked` in `internal/assistant/tools.go`), and repeated calls within one reply are skipped.

## Your Grok Bot agent

The TV can instead be the voice of one of your own [Grok Bot](https://docs.x.ai/grok-bot) agents, for example a chief of staff with your email and calendar. With `grok_bot` set, everything you say goes to that agent as a message in its thread, and everything it says back is shown under its name and read aloud. Grok no longer answers; xAI only transcribes your speech and voices the agent's replies. Conversations work as before: after a reply, grokbot listens for a few seconds. Messages the agent sends later (it often says "on it" first and the result afterwards) are read out as soon as the TV is free, and only while the screen is on. Everything also appears in the agent's thread in the Grok Bot app.

The first message of each conversation tells the agent it is speaking through the TV: keep replies to a few spoken sentences, and control the TV with commands on their own line. The TV carries these out instead of reading them aloud:

| Command | |
|---|---|
| `[[tv open Netflix]]` | Open an app |
| `[[tv input hdmi1]]` | Switch input (`hdmi1`–`hdmi4`, `tv`) |
| `[[tv volume 30]]`, `[[tv volume +5]]` | Set or change the volume |
| `[[tv mute]]`, `[[tv unmute]]` | |
| `[[tv look]]` | Take a screenshot; Grok describes it and the description goes back to the agent, which only takes text |

There is no public Grok Bot API. `internal/botapi` makes the same calls as the community [grok-bot-cli](https://github.com/ScriptedAlchemy/grok-bot-cli): the Grok Bot app's access token is exchanged for the gateway of your account's cloud computer (`EnsureSandBox` on `api2.cursor.sh`, which also wakes it). The gateway serves `sendPrompt` and `getAgentTranscriptTail`. Each message carries a client nonce, which leads to its request id, and the agent's `send-message` entries with that request id are its answers. The transcript is polled every second for the first minute. The backend refuses outdated client versions, so the app's version is sent along. Undocumented endpoints can change without notice.

Setup:

1. Find the agent's id: `gbot bots list` with grok-bot-cli, or the agent's link in the app.
2. Add it to `config.json`: `"grok_bot": {"name": "David", "agent_id": "…"}`.
3. Copy the sign-in to the TV from a Mac that runs the Grok Bot app: `TV=root@<tv> ./scripts/sync-grok-bot.sh`. It goes to `/home/root/.config/grokbot/grok-bot-auth.json` (mode 600) and is read on every message, so no restart is needed.

The sign-in lasts a few weeks. The TV never renews it (renewing would likely sign the app out); when it expires, grokbot says so, and you run the sync script again. It lets anyone with root on the TV act through your agent, so set a root password on the TV that isn't the default.

## How it works on webOS 6

- **Voice button.** `lginput2` logs every remote click to `/var/log/messages` (`NL_BUTTON_CLICK {"button_type":"KEY_VOICE"}`). The daemon follows that file. There is no input event for the key, since webOS handles it internally.
- **LG's assistant.** `voiceconductor` turns the key into LG's voice UI. grokbot stops `voiceconductor.service` while it runs and restarts it when the daemon exits. Set `replace_lg_voice: false` to leave it alone.
- **Microphone.** `luna://com.webos.service.voiceinput/startStreaming {"deviceType":"remote","keyType":"thinQtv","subscribe":true}` turns the remote's mic on and returns a unix socket (`/tmp/voiceinput_remote1.soc`). The socket delivers 16 kHz 16-bit mono PCM for as long as the subscription is held open. The remote only shows up as a voice device while it's awake, which pressing the button takes care of.
- **Speech output.** Replies are decoded with GStreamer and played with `pacat` on PulseAudio's `ptts` sink, the channel LG's audio guidance uses. It mixes over the current input, including an HDMI ARC soundbar. Don't play audio from the overlay web app: webOS hands the main audio route to the app, and the HDMI input stays disconnected after the overlay closes. In that case, closing and relaunching the HDMI app reconnects it.
- **Overlay.** The app is declared with `"defaultWindowType": "overlay"`, like LG's own voice and Alexa UIs. It floats over the current app, which keeps playing and is still there when the overlay closes.

## Face

The face is the `<morph-bot>` web component, with 39 states. It follows the session: `listening` while you talk, `dictating` while it transcribes, `thinking`, and `working` with a "send" morph when Grok uses a tool. Grok starts each reply with a mood tag (`[curious]`, `[laughing]`, …), which the daemon strips from the text and sends to the overlay as the face's expression. While the answer is spoken, the face bounces with the loudness of the voice, which the daemon sends every 80 ms.

The TV's browser is Chromium 79. `build-face.sh` bundles the component with esbuild for `chrome79` and adds polyfills for `Array.prototype.at`, `Object.hasOwn` and `replaceAll`. It also drops the component's sampled-voice dialogue system (about 2 MB) and patches out a per-frame forced layout. On a C1 the face renders at about 40 fps.

The component's geometry is derived from x.ai's public site and comes without a license (see its README), so review that before redistributing a build.

## Requirements

- A rooted LG TV with [Homebrew Channel](https://github.com/webosbrew/webos-homebrew-channel) and SSH. Developed on an OLED77C1 (webOS 6.5).
- An xAI API key from [console.x.ai](https://console.x.ai). X developer keys won't work.
- Go 1.24+ and Node (for `ares-package` and esbuild) on the build machine.

## Install

```sh
./scripts/build-face.sh                                   # only when grokbot-animation changes
./scripts/build.sh                                        # build/grokbotd + overlay .ipk
XAI_API_KEY=xai-... TV=root@<tv> ./scripts/deploy.sh
```

`deploy.sh` copies everything to `/home/root/grokbot`. It installs the overlay app and adds `/var/lib/webosbrew/init.d/50-grokbot`, so the daemon starts at boot. On first install it writes `/home/root/.config/grokbot/config.json`. To apply changes, edit that file and run `sh /home/root/grokbot/run.sh stop; sh /home/root/grokbot/run.sh start`.

On the TV:

```sh
tail -f /tmp/grokbot.log                       # sessions, transcripts, answers, tool calls
wget -qO- http://127.0.0.1:8679/status         # busy, overlays connected
wget -qO- --post-data= http://127.0.0.1:8679/trigger   # start a session without the button (the remote must be awake)
luna-send -n 1 luna://com.webos.applicationManager/launch '{"id":"com.grokbot.overlay","params":{"demo":true}}'
                                               # cycle the face through its states and show the frame rate
```

To uninstall: run `sh /home/root/grokbot/run.sh stop` (this restores LG's assistant), then `rm -r /home/root/grokbot /var/lib/webosbrew/init.d/50-grokbot`, and remove the "Grokbot" app.

## Config

`config.json` (see `config.example.json`):

| Key | Default | |
|---|---|---|
| `xai_api_key` | | Required. |
| `chat_model` / `stt_model` | `grok-4.7` / `grok-voice-transcribe-2.0` | |
| `tts`, `tts_voice` | `true`, `iris` | Spoken replies. Voices include `leo`, `iris`, `luna`, `orion`, `carina`, `atlas`, … |
| `replace_lg_voice` | `true` | Stop LG's voice assistant while grokbot runs. |
| `mute_while_listening` | `false` | Mutes the TV while recording. The remote mic is close to your mouth, so this is rarely needed. With sound going to an HDMI ARC soundbar, muting goes over CEC and can leave the soundbar muted. |
| `end_silence_sec`, `max_listen_sec`, `no_speech_sec` | `0.9`, `12`, `5` | Endpointing. Pressing Voice again also ends the recording. |
| `follow_up_sec` | `6` | After answering, keep listening this long for a follow-up, with no press needed. `0` turns conversations off; the answer then stays up for `linger_sec`. |
| `min_speech_rms` | `300` | Speech gate. The remote mic idles around 50–150 and speech reaches the thousands. |
| `grok_bot` | off | `name`, `agent_id`, `auth_file`: the Grok Bot agent the TV speaks for (see above). |

## Development

```sh
go test ./...
node --check overlay/js/app.js
```

The overlay can be inspected with Chrome DevTools at `http://<tv>:9998` while it's open.

## Notes and limits

- `luna-send` only prints its reply when stdin isn't the SSH channel. The daemon runs it with `/dev/null` as stdin.
- `/tmp/voiceinput_disable_set_input_event`, which older reference code uses to suppress LG's UI, has no effect on webOS 6.5.
- Only button presses are visible, not releases, so recording ends on silence, a second press, or `max_listen_sec`.
- One press starts a conversation, and the remote mic stays on until it ends. Closing and reopening the mic leaves voiceinput "already capturing" for a few seconds, so follow-ups reuse the open stream. A press while Grok answers interrupts it and listens again.
- Reference: [gprot42/webos-launch-home](https://github.com/gprot42/webos-launch-home).

## License

MIT, see [LICENSE](LICENSE). This doesn't cover `overlay/js/vendor/morph-bot.js`, which is built from [grokbot-animation](https://github.com/iduu/grokbot-animation) and has no license (see Face).
