package speaker

import (
	"context"
	"os"
	"testing"
)

func TestEnvelope(t *testing.T) {
	pcm := make([]int16, rate) // 1 s: half silence, half loud
	for i := rate / 2; i < rate; i++ {
		pcm[i] = 8000
	}
	levels := envelope(pcm)
	if len(levels) != 13 {
		t.Fatalf("got %d levels, want 13 (80 ms each)", len(levels))
	}
	if levels[0] != 0 || levels[len(levels)-1] < 0.9 {
		t.Errorf("levels %v", levels)
	}
}

// TestPlayOnTV plays $SPEAKER_MP3 through the TV; it only runs on the TV.
func TestPlayOnTV(t *testing.T) {
	path := os.Getenv("SPEAKER_MP3")
	if path == "" {
		t.Skip("set SPEAKER_MP3 to an MP3 file (on the TV)")
	}
	mp3, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n, peak := 0, 0.0
	if err := Play(context.Background(), mp3, func(l float64) { n++; peak = max(peak, l) }); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d level updates, peak %.2f", n, peak)
	if n < 10 || peak < 0.2 {
		t.Errorf("levels look wrong: %d updates, peak %.2f", n, peak)
	}
}
