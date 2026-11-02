// grokbotd is a push-to-talk voice assistant daemon for rooted LG webOS TVs:
// the Magic Remote's Voice button and microphone, a webOS overlay app, and
// Grok (xAI) for transcription, answers and speech.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dynjo/grokbot-tv/internal/assistant"
	"github.com/dynjo/grokbot-tv/internal/bridge"
	"github.com/dynjo/grokbot-tv/internal/remote"
)

func main() {
	configPath := flag.String("config", "/home/root/.config/grokbot/config.json", "config file")
	flag.Parse()
	log.SetFlags(log.Ldate | log.Ltime)

	cfg, err := assistant.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config %s: %v", *configPath, err)
	}
	if cfg.XAIAPIKey == "" {
		log.Printf("warning: xai_api_key is empty in %s; sessions will fail", *configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := bridge.NewHub()
	asst := assistant.New(cfg, hub)
	hub.Handle("/trigger", bridge.LocalOnly(func(w http.ResponseWriter, r *http.Request) {
		asst.Trigger("http")
		fmt.Fprintln(w, `{"ok":true}`)
	}))
	hub.Handle("/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"service":          "grokbot",
			"busy":             asst.Busy(),
			"replace_lg_voice": cfg.ReplaceLGVoice,
			"overlays":         hub.Clients(),
		})
	})
	go func() {
		if err := hub.Listen(cfg.ListenAddr); err != nil {
			log.Fatalf("listen %s: %v", cfg.ListenAddr, err)
		}
	}()

	suppressed := make(chan struct{})
	if cfg.ReplaceLGVoice {
		go func() {
			remote.SuppressLG(ctx)
			close(suppressed)
		}()
	} else {
		close(suppressed)
	}

	presses := make(chan time.Time, 1)
	go remote.WatchVoiceKey(ctx, presses)
	log.Printf("ready: hold the Voice button on the Magic Remote")
	asst.Run(ctx, presses)
	<-suppressed // LG's assistant is restored before exiting
	log.Printf("stopped")
}
