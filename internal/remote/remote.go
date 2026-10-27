// Package remote handles the Magic Remote: Voice button presses, LG's own
// voice assistant, and the remote's microphone.
package remote

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Rate is the sample rate of the remote microphone (16-bit mono).
const Rate = 16000

const (
	syslogPath   = "/var/log/messages"
	conductor    = "voiceconductor.service"
	streamURI    = "luna://com.webos.service.voiceinput/startStreaming"
	streamParams = `{"deviceType":"remote","keyType":"thinQtv","subscribe":true}`
	stopURI      = "luna://com.webos.service.voiceinput/stopStreaming"
	stopParams   = `{"deviceType":"remote"}`
	mrcuURI      = "luna://com.webos.service.mrcu/activateMrcu"
	// defaultSocket is where webOS 6 puts the remote mic when the
	// startStreaming reply has no socketPath.
	defaultSocket = "/tmp/voiceinput_remote1.soc"
)

// WatchVoiceKey sends on presses each time the Voice button is pressed.
// lginput2 logs every remote button click to syslog, for example
//
//	lginput2 NL_BUTTON_CLICK {"remote_type":"LGE MR21N","button_type":"KEY_VOICE"}
//
// The file is followed across log rotation.
func WatchVoiceKey(ctx context.Context, presses chan<- time.Time) {
	var (
		f    *os.File
		r    *bufio.Reader
		info os.FileInfo
		pos  int64
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if f == nil {
			var err error
			if f, err = os.Open(syslogPath); err == nil {
				info, _ = f.Stat()
				pos, _ = f.Seek(0, io.SeekEnd)
				r = bufio.NewReader(f)
			}
		}
		for f != nil {
			line, err := r.ReadString('\n')
			pos += int64(len(line))
			if err != nil {
				// Partial line: rewind so it is read whole next time.
				pos -= int64(len(line))
				f.Seek(pos, io.SeekStart)
				r.Reset(f)
				break
			}
			if strings.Contains(line, "KEY_VOICE") && strings.Contains(line, "BUTTON_CLICK") {
				select {
				case presses <- time.Now():
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		// Rotated or truncated: start over on the new file.
		if f != nil {
			cur, err := os.Stat(syslogPath)
			if err != nil || !os.SameFile(info, cur) || cur.Size() < pos {
				f.Close()
				f = nil
				if err == nil {
					if f, err = os.Open(syslogPath); err == nil {
						info, _ = f.Stat()
						pos = 0
						r = bufio.NewReader(f)
					}
				}
			}
		}
	}
}

// SuppressLG keeps LG's voice conductor stopped until ctx is done, then
// starts it again. Without the conductor the Voice button no longer opens
// LG's assistant, but voiceinput (which owns the remote mic) keeps running.
func SuppressLG(ctx context.Context) {
	stop := func() {
		out, _ := exec.Command("systemctl", "is-active", conductor).Output()
		if strings.TrimSpace(string(out)) == "inactive" {
			return
		}
		if err := exec.Command("systemctl", "stop", conductor).Run(); err != nil {
			log.Printf("remote: stop %s: %v", conductor, err)
			return
		}
		log.Printf("remote: stopped %s (LG voice assistant)", conductor)
	}
	stop()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := exec.Command("systemctl", "start", conductor).Run(); err != nil {
				log.Printf("remote: start %s: %v", conductor, err)
			} else {
				log.Printf("remote: restarted %s", conductor)
			}
			return
		case <-t.C:
			stop()
		}
	}
}

// Mic is an open remote microphone stream. A reader drains the socket in
// the background so the stream never backs up while nobody is listening.
type Mic struct {
	sub    *exec.Cmd
	conn   net.Conn
	once   sync.Once
	chunks chan []int16
	err    error // set before chunks is closed
}

// OpenMic powers the remote microphone through voiceinput and connects to
// its PCM socket. voiceinput only knows the remote while it is awake, so the
// request is retried until wait has passed. The mic stays on while the
// startStreaming subscription is held open.
func OpenMic(ctx context.Context, wait time.Duration) (*Mic, error) {
	deadline := time.Now().Add(wait)
	woke := false
	for {
		m, reply, err := subscribe(ctx)
		if err == nil {
			return m, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			if reply != "" {
				return nil, fmt.Errorf("remote mic: %s", reply)
			}
			return nil, fmt.Errorf("remote mic: %v", err)
		}
		if !woke {
			exec.Command("luna-send", "-n", "1", "-w", "2000", mrcuURI, `{}`).Run()
			woke = true
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func subscribe(ctx context.Context) (*Mic, string, error) {
	cmd := exec.Command("luna-send", "-i", streamURI, streamParams)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	m := &Mic{sub: cmd}
	lines := bufio.NewScanner(stdout)
	first := make(chan string, 1)
	go func() {
		if lines.Scan() {
			first <- lines.Text()
		}
		close(first)
		for lines.Scan() { // keep the pipe drained
		}
	}()
	var line string
	select {
	case line = <-first:
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
	}
	var reply struct {
		ReturnValue bool   `json:"returnValue"`
		SocketPath  string `json:"socketPath"`
		ErrorText   string `json:"errorText"`
	}
	json.Unmarshal([]byte(line), &reply)
	if strings.Contains(reply.ErrorText, "already capturing") {
		// A capture that just ended is still winding down (it takes
		// voiceinput a few seconds). Its socket keeps delivering audio,
		// so join it rather than wait.
		if conn, err := net.Dial("unix", defaultSocket); err == nil {
			m.conn = conn
			m.start()
			return m, "", nil
		}
	}
	if !reply.ReturnValue {
		m.Close()
		if reply.ErrorText != "" {
			return nil, reply.ErrorText, fmt.Errorf("%s", reply.ErrorText)
		}
		return nil, strings.TrimSpace(line), fmt.Errorf("startStreaming: no reply")
	}
	path := reply.SocketPath
	if path == "" {
		path = defaultSocket
	}
	for i := 0; ; i++ {
		if m.conn, err = net.Dial("unix", path); err == nil {
			m.start()
			return m, "", nil
		}
		if i == 40 || ctx.Err() != nil {
			m.Close()
			return nil, "", err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ErrEnded is returned by Read once voiceinput has ended the stream. The
// remote stops sending audio by itself after roughly 15 seconds.
var ErrEnded = errors.New("remote mic: stream ended")

// ChunkSamples is the size of the chunks Read returns (80 ms).
const ChunkSamples = Rate * 80 / 1000

// start reads the socket into chunks, dropping the oldest audio once about
// ten seconds are queued.
func (m *Mic) start() {
	m.chunks = make(chan []int16, 128)
	go func() {
		defer close(m.chunks)
		buf := make([]byte, 2*ChunkSamples)
		for {
			if _, err := io.ReadFull(m.conn, buf); err != nil {
				m.err = err
				return
			}
			pcm := make([]int16, ChunkSamples)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(buf[2*i:]))
			}
			for {
				select {
				case m.chunks <- pcm:
				default:
					select {
					case <-m.chunks:
					default:
					}
					continue
				}
				break
			}
		}
	}()
}

// Read returns the next 80 ms chunk, waiting up to timeout for it.
func (m *Mic) Read(timeout time.Duration) ([]int16, error) {
	select {
	case pcm, ok := <-m.chunks:
		if !ok {
			return nil, ErrEnded
		}
		return pcm, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("remote mic: no audio for %v", timeout)
	}
}

// Flush discards queued audio, such as Grok's own voice from the TV
// speakers.
func (m *Mic) Flush() {
	for {
		select {
		case _, ok := <-m.chunks:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// Close turns the microphone off.
func (m *Mic) Close() {
	m.once.Do(func() {
		if m.conn != nil {
			m.conn.Close()
		}
		// Ending the subscription stops the stream; stopStreaming from
		// another client is refused ("permission fail"), so it is only a
		// fallback. SIGINT first gives luna-send a chance to cancel the
		// call cleanly.
		m.sub.Process.Signal(os.Interrupt)
		exited := make(chan struct{})
		go func() { m.sub.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(300 * time.Millisecond):
			m.sub.Process.Kill()
			<-exited
		}
		exec.Command("luna-send", "-n", "1", "-w", "2000", stopURI, stopParams).Run()
	})
}

// RMS is the root mean square level of pcm.
func RMS(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}
