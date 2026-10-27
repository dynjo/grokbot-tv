package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Luna calls a Luna bus method with luna-send and decodes the reply.
// luna-send only prints its reply when stdin is not a terminal or the SSH
// channel, which exec's default /dev/null stdin satisfies.
func Luna(ctx context.Context, uri string, params any) (map[string]any, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "luna-send", "-n", "1", "-w", "8000", uri, string(body)).Output()
	if err != nil {
		return nil, fmt.Errorf("luna %s: %v", uri, err)
	}
	var reply map[string]any
	if err := json.Unmarshal(out, &reply); err != nil {
		return nil, fmt.Errorf("luna %s: bad reply %q", uri, strings.TrimSpace(string(out)))
	}
	if ok, _ := reply["returnValue"].(bool); !ok {
		return reply, fmt.Errorf("luna %s: %v", uri, reply["errorText"])
	}
	return reply, nil
}

// Launch starts or relaunches an app.
func Launch(ctx context.Context, appID string, params map[string]any) error {
	p := map[string]any{"id": appID}
	if params != nil {
		p["params"] = params
	}
	_, err := Luna(ctx, "luna://com.webos.applicationManager/launch", p)
	return err
}

// Close closes an app.
func Close(ctx context.Context, appID string) error {
	_, err := Luna(ctx, "luna://com.webos.applicationManager/closeByAppId", map[string]any{"id": appID})
	return err
}

// ForegroundApp returns the id of the app on screen.
func ForegroundApp(ctx context.Context) string {
	r, err := Luna(ctx, "luna://com.webos.applicationManager/getForegroundAppInfo", map[string]any{})
	if err != nil {
		return ""
	}
	id, _ := r["appId"].(string)
	return id
}

// App is an installed, visible app.
type App struct {
	ID    string
	Title string
}

// Apps lists the visible installed apps (needs root).
func Apps(ctx context.Context) ([]App, error) {
	r, err := Luna(ctx, "luna://com.webos.applicationManager/listApps", map[string]any{})
	if err != nil {
		return nil, err
	}
	list, _ := r["apps"].([]any)
	var apps []App
	for _, a := range list {
		m, _ := a.(map[string]any)
		id, _ := m["id"].(string)
		title, _ := m["title"].(string)
		if visible, _ := m["visible"].(bool); id == "" || title == "" || !visible {
			continue
		}
		apps = append(apps, App{ID: id, Title: title})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Title < apps[j].Title })
	return apps, nil
}

// Volume returns the master volume and mute state.
func Volume(ctx context.Context) (level int, muted bool, err error) {
	r, err := Luna(ctx, "luna://com.webos.service.audio/master/getVolume", map[string]any{})
	if err != nil {
		return 0, false, err
	}
	st, _ := r["volumeStatus"].(map[string]any)
	v, _ := st["volume"].(float64)
	m, _ := st["muteStatus"].(bool)
	return int(v), m, nil
}

// SetVolume sets the master volume (0-100).
func SetVolume(ctx context.Context, level int) error {
	_, err := Luna(ctx, "luna://com.webos.service.audio/master/setVolume", map[string]any{"volume": max(0, min(100, level))})
	return err
}

// SetMuted mutes or unmutes the master output.
func SetMuted(ctx context.Context, muted bool) error {
	_, err := Luna(ctx, "luna://com.webos.service.audio/master/muteVolume", map[string]any{"mute": muted})
	return err
}

// Screenshot captures what is on screen (video and UI) as a JPEG.
func Screenshot(ctx context.Context, width, height int) ([]byte, error) {
	path := fmt.Sprintf("/tmp/grokbot-screen-%d.jpg", time.Now().UnixNano())
	defer os.Remove(path)
	_, err := Luna(ctx, "luna://com.webos.service.capture/executeOneShot", map[string]any{
		"path": path, "method": "DISPLAY", "format": "JPEG", "width": width, "height": height,
	})
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// PowerState is the TV's power state: "Active" when the screen is on, or
// for example "Screen Off" or "Active Standby". It is "" if unknown.
func PowerState(ctx context.Context) string {
	r, err := Luna(ctx, "luna://com.webos.service.tvpower/power/getPowerState", map[string]any{})
	if err != nil {
		return ""
	}
	state, _ := r["state"].(string)
	return state
}
