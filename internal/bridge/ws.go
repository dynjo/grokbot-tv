// Package bridge connects the daemon to the TV: a localhost WebSocket for
// the overlay app, and Luna bus calls through luna-send.
package bridge

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Hub is a WebSocket server that broadcasts JSON events to every connected
// overlay. Events of the current session are replayed to late joiners, since
// the overlay app connects a moment after the daemon launches it.
type Hub struct {
	OnMessage func(msg map[string]any)

	mux     *http.ServeMux
	mu      sync.Mutex
	clients map[*wsConn]bool
	replay  [][]byte
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	h := &Hub{clients: map[*wsConn]bool{}, mux: http.NewServeMux()}
	h.mux.HandleFunc("/", h.serveWS)
	return h
}

// Handle adds an HTTP endpoint next to the WebSocket.
func (h *Hub) Handle(path string, f http.HandlerFunc) { h.mux.HandleFunc(path, f) }

// Listen serves the hub on addr (e.g. 127.0.0.1:8679).
func (h *Hub) Listen(addr string) error {
	return http.ListenAndServe(addr, h.mux)
}

// Clients returns how many overlays are connected.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// ClearReplay empties the replay log. Call it when a voice session starts
// and after it closes, so a later launch does not replay a finished session.
func (h *Hub) ClearReplay() {
	h.mu.Lock()
	h.replay = nil
	h.mu.Unlock()
}

// Send broadcasts {"event": event, ...fields}. Events with replay set are
// also kept for overlays that connect later in the session.
func (h *Hub) Send(event string, fields map[string]any, replay bool) {
	msg := map[string]any{"event": event}
	for k, v := range fields {
		msg[k] = v
	}
	buf, err := json.Marshal(msg)
	if err != nil {
		log.Printf("bridge: marshal %s: %v", event, err)
		return
	}
	h.mu.Lock()
	if replay {
		h.replay = append(h.replay, buf)
	}
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		if err := c.writeText(buf); err != nil {
			h.drop(c)
		}
	}
}

func (h *Hub) drop(c *wsConn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.conn.Close()
}

// overlayOrigin reports whether a WebSocket comes from the overlay app.
// webOS web apps are loaded from file://; web pages in the TV's browser
// (which can also reach 127.0.0.1) have an http(s) origin and are refused,
// since the socket carries what the user says and can control sessions.
func overlayOrigin(origin string) bool {
	return origin == "file://" || origin == "null"
}

// LocalOnly wraps a handler for tools on the TV itself (wget, curl): it
// only takes POST, and refuses requests from web pages, which carry an
// Origin header.
func LocalOnly(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Origin") != "" {
			http.Error(w, "POST from the TV only", http.StatusForbidden)
			return
		}
		f(w, r)
	}
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"service":"grokbot"}`)
		return
	}
	if origin := r.Header.Get("Origin"); !overlayOrigin(origin) {
		log.Printf("bridge: refused WebSocket from origin %q", origin)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		conn.Close()
		return
	}
	c := &wsConn{conn: conn, r: rw.Reader}
	h.mu.Lock()
	h.clients[c] = true
	replay := append([][]byte(nil), h.replay...)
	h.mu.Unlock()
	for _, m := range replay {
		if c.writeText(m) != nil {
			h.drop(c)
			return
		}
	}
	go h.readLoop(c)
}

func (h *Hub) readLoop(c *wsConn) {
	defer h.drop(c)
	for {
		op, payload, err := c.readFrame()
		if err != nil {
			return
		}
		switch op {
		case 0x1: // text
			var msg map[string]any
			if json.Unmarshal(payload, &msg) == nil && h.OnMessage != nil {
				h.OnMessage(msg)
			}
		case 0x8: // close
			c.writeFrame(0x8, nil)
			return
		case 0x9: // ping
			c.writeFrame(0xA, payload)
		}
	}
}

type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex
}

func (c *wsConn) writeText(p []byte) error { return c.writeFrame(0x1, p) }

func (c *wsConn) writeFrame(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// A stuck client must not block broadcasts to the others.
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	hdr := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 1<<16:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	if _, err := c.conn.Write(hdr); err != nil {
		return err
	}
	_, err := c.conn.Write(p)
	return err
}

// readFrame reads one (possibly fragmented) client message.
func (c *wsConn) readFrame() (byte, []byte, error) {
	var msg []byte
	var opcode byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return 0, nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 1<<20 || uint64(len(msg))+n > 1<<20 {
			return 0, nil, errors.New("ws: message too large")
		}
		var mask [4]byte
		if h[1]&0x80 != 0 {
			if _, err := io.ReadFull(c.r, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.r, p); err != nil {
			return 0, nil, err
		}
		for i := range p {
			p[i] ^= mask[i%4]
		}
		if op >= 0x8 { // control frames are never fragmented
			return op, p, nil
		}
		if op != 0 {
			opcode = op
		}
		msg = append(msg, p...)
		if fin {
			return opcode, msg, nil
		}
	}
}
