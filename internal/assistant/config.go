// Package assistant runs the voice session: Voice button -> overlay ->
// record the remote mic until silence -> transcribe -> answer (with TV
// tools) -> speak.
package assistant

import (
	"encoding/json"
	"os"
)

// Config is the daemon configuration (config.json).
type Config struct {
	XAIAPIKey    string `json:"xai_api_key"`
	ChatModel    string `json:"chat_model"`
	STTModel     string `json:"stt_model"`
	Language     string `json:"language"`
	TTS          bool   `json:"tts"`
	TTSVoice     string `json:"tts_voice"`
	SystemPrompt string `json:"system_prompt"`

	// ReplaceLGVoice stops LG's voice assistant while grokbot runs, so the
	// Voice button only opens grokbot.
	ReplaceLGVoice bool `json:"replace_lg_voice"`

	MuteWhileListening bool    `json:"mute_while_listening"`
	EndSilenceSec      float64 `json:"end_silence_sec"`
	MaxListenSec       float64 `json:"max_listen_sec"`
	NoSpeechSec        float64 `json:"no_speech_sec"`
	MinSpeechRMS       float64 `json:"min_speech_rms"`
	LingerSec          float64 `json:"linger_sec"`    // keep the answer up (no follow-up)
	FollowUpSec        float64 `json:"follow_up_sec"` // listen for a follow-up after answering
	HistoryMinutes     float64 `json:"history_minutes"`

	// GrokBot is the user's own Grok Bot agent (such as a chief of staff).
	// Requests about their life outside the TV are handed to it, and its
	// replies are read out.
	GrokBot GrokBotConfig `json:"grok_bot"`

	OverlayApp string `json:"overlay_app"`
	ListenAddr string `json:"listen_addr"`
}

// GrokBotConfig names the agent. The sign-in lives in AuthFile, written by
// scripts/sync-grok-bot.sh from the Grok Bot app on a Mac, and is read on
// every request, so a new copy needs no restart.
type GrokBotConfig struct {
	Name     string `json:"name"`     // what the user calls it, e.g. "David"
	AgentID  string `json:"agent_id"` // from `gbot bots list`
	AuthFile string `json:"auth_file"`
}

// Defaults returns the default configuration.
func Defaults() Config {
	return Config{
		ChatModel:          "grok-4.7",
		STTModel:           "grok-voice-transcribe-2.0",
		Language:           "en",
		TTS:                true,
		TTSVoice:           "iris",
		ReplaceLGVoice:     true,
		MuteWhileListening: false, // muting can leave an HDMI ARC soundbar muted
		EndSilenceSec:      0.9,
		MaxListenSec:       12,
		NoSpeechSec:        5,
		MinSpeechRMS:       300,
		LingerSec:          6,
		FollowUpSec:        6,
		HistoryMinutes:     3,
		GrokBot: GrokBotConfig{
			AuthFile: "/home/root/.config/grokbot/grok-bot-auth.json",
		},
		OverlayApp: "com.grokbot.overlay",
		ListenAddr: "127.0.0.1:8679",
	}
}

// LoadConfig reads path over the defaults. A missing file gives the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := Defaults()
	buf, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(buf, &cfg)
	return cfg, err
}
