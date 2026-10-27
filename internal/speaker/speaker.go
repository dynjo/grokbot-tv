// Package speaker plays speech on the TV.
//
// Audio goes to PulseAudio's "ptts" sink, the one LG's own audio guidance
// uses, which is mixed over whatever is playing. Playing it from the overlay
// web app instead hands the TV's main audio route to the app and leaves the
// HDMI input disconnected after the overlay closes.
package speaker

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	rate      = 24000
	sink      = "ptts"
	pulse     = "unix:/var/run/pulse/native"
	levelStep = 80 * time.Millisecond
)

// Play decodes mp3 and plays it, calling onLevel with the loudness (0–1)
// every 80 ms in step with playback. It returns when playback ends or ctx is
// cancelled.
func Play(ctx context.Context, mp3 []byte, onLevel func(float64)) error {
	pcm, err := decode(ctx, mp3)
	if err != nil {
		return err
	}
	levels := envelope(pcm)

	raw, err := os.CreateTemp("", "grokbot-tts-*.raw")
	if err != nil {
		return err
	}
	defer os.Remove(raw.Name())
	defer raw.Close()
	if err := binary.Write(raw, binary.LittleEndian, pcm); err != nil {
		return err
	}
	raw.Seek(0, 0)

	cmd := exec.CommandContext(ctx, "pacat", "--playback", "--device="+sink, "--raw",
		"--format=s16le", fmt.Sprintf("--rate=%d", rate), "--channels=1", "--latency-msec=60")
	cmd.Env = append(os.Environ(), "PULSE_SERVER="+pulse)
	cmd.Stdin = raw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	tick := time.NewTicker(levelStep)
	defer tick.Stop()
	for i := 0; ; {
		select {
		case err := <-done:
			onLevel(0)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		case <-tick.C:
			if i < len(levels) {
				onLevel(levels[i])
				i++
			}
		}
	}
}

// decode turns MP3 into 24 kHz mono PCM with GStreamer.
func decode(ctx context.Context, mp3 []byte) ([]int16, error) {
	in, err := os.CreateTemp("", "grokbot-tts-*.mp3")
	if err != nil {
		return nil, err
	}
	defer os.Remove(in.Name())
	if _, err := in.Write(mp3); err != nil {
		in.Close()
		return nil, err
	}
	in.Close()
	out := strings.TrimSuffix(in.Name(), ".mp3") + ".pcm"
	defer os.Remove(out)
	pipeline := fmt.Sprintf("filesrc location=%s ! decodebin ! audioconvert ! audioresample ! "+
		"audio/x-raw,format=S16LE,channels=1,rate=%d ! filesink location=%s", in.Name(), rate, out)
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", append([]string{"-q"}, strings.Fields(pipeline)...)...)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("speaker: decode: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	buf, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	pcm := make([]int16, len(buf)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
	}
	return pcm, nil
}

// envelope returns the loudness of each 80 ms of pcm, scaled so normal
// speech peaks near 1.
func envelope(pcm []int16) []float64 {
	n := rate * int(levelStep/time.Millisecond) / 1000
	var levels []float64
	for i := 0; i < len(pcm); i += n {
		chunk := pcm[i:min(i+n, len(pcm))]
		var sum float64
		for _, s := range chunk {
			sum += float64(s) * float64(s)
		}
		rms := math.Sqrt(sum/float64(len(chunk))) / 32768
		levels = append(levels, min(1, rms*5))
	}
	return levels
}
