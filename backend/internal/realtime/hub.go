// Package realtime provides the WebSocket layer that pushes player movement,
// chunk invalidation and feature updates to connected browsers.
//
// The WebSocket protocol is implemented directly against RFC 6455 rather than
// pulling in a dependency. The server's needs are narrow -- accept an upgrade,
// push small text frames, honour ping and close -- and implementing them
// directly keeps the module free of third-party network code.
package realtime

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// websocketGUID is the fixed value RFC 6455 specifies for the accept hash.
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes used by this implementation.
const (
	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

// Event is a message pushed to clients.
type Event struct {
	Type string `json:"type"`
	// Payload fields are inlined by the concrete event constructors below.
	Dimension string `json:"dimension,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// EventPlayerMove announces player positions.
func EventPlayerMove(dimension string, players any) Event {
	return Event{Type: "player.move", Dimension: dimension, Data: players}
}

// EventChunkUpdated announces that a chunk changed and which tiles it dirtied.
//
// The client uses this to bump the revision of exactly the affected tiles, so
// it refetches a handful of URLs instead of dropping its whole tile cache.
func EventChunkUpdated(dimension string, chunkX, chunkZ int, revision uint64, tiles any) Event {
	return Event{
		Type:      "chunk.updated",
		Dimension: dimension,
		Data: map[string]any{
			"chunkX":   chunkX,
			"chunkZ":   chunkZ,
			"revision": revision,
			"tiles":    tiles,
		},
	}
}

// EventFeatureUpdated announces a change to claims, regions or markers.
func EventFeatureUpdated(dimension, kind string, data any) Event {
	return Event{Type: kind + ".updated", Dimension: dimension, Data: data}
}

// client is one connected browser.
type client struct {
	conn net.Conn
	send chan []byte
	hub  *Hub
	once sync.Once
	// done is closed exactly once, by close(), to signal writeLoop/readLoop to
	// stop. send is never closed: Broadcast may be sending to it concurrently
	// from another goroutine, and closing a channel a sender might still write
	// to is a send-on-closed-channel panic waiting to happen.
	done chan struct{}

	// dimension filters events so a client watching the Nether is not woken by
	// overworld player movement.
	mu        sync.RWMutex
	dimension string
}

// Hub fans events out to all connected clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}

	maxConns int
	log      *slog.Logger

	connections atomic.Int64
	sent        atomic.Int64
	dropped     atomic.Int64
}

// NewHub creates a hub.
func NewHub(maxConns int, log *slog.Logger) *Hub {
	if maxConns <= 0 {
		maxConns = 500
	}
	if log == nil {
		log = slog.Default()
	}
	return &Hub{clients: make(map[*client]struct{}), maxConns: maxConns, log: log}
}

// Connections reports the current client count.
func (h *Hub) Connections() int { return int(h.connections.Load()) }

// Broadcast sends an event to every client watching a dimension. An empty
// dimension reaches everyone.
//
// Sends are non-blocking: a client whose buffer is full is disconnected rather
// than allowed to stall the broadcaster. One slow browser must never be able to
// back up the world-update pipeline for everyone else.
func (h *Hub) Broadcast(ev Event) {
	raw, err := json.Marshal(ev)
	if err != nil {
		h.log.Error("cannot encode realtime event", "type", ev.Type, "error", err)
		return
	}

	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if ev.Dimension != "" {
			c.mu.RLock()
			watching := c.dimension
			c.mu.RUnlock()
			if watching != "" && watching != ev.Dimension {
				continue
			}
		}
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		select {
		case c.send <- raw:
			h.sent.Add(1)
		default:
			h.dropped.Add(1)
			c.close()
		}
	}
}

// ServeHTTP upgrades an HTTP request to a WebSocket connection.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	if int(h.connections.Load()) >= h.maxConns {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "cannot hijack connection", http.StatusInternalServerError)
		return
	}

	accept := acceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		conn.Close()
		return
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return
	}

	c := &client{
		conn: conn,
		send: make(chan []byte, 64),
		hub:  h,
		done: make(chan struct{}),
		// Default to the dimension the client asked for in the query string, so
		// the first frame is already filtered correctly.
		dimension: r.URL.Query().Get("dimension"),
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.connections.Add(1)

	go c.writeLoop()
	go c.readLoop()
}

// acceptKey computes the RFC 6455 handshake response value.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// close removes a client exactly once.
func (c *client) close() {
	c.once.Do(func() {
		c.hub.mu.Lock()
		delete(c.hub.clients, c)
		c.hub.mu.Unlock()
		c.hub.connections.Add(-1)
		close(c.done)
		_ = c.conn.Close()
	})
}

// writeLoop drains the send queue and keeps the connection alive with pings.
func (c *client) writeLoop() {
	ping := time.NewTicker(30 * time.Second)
	defer func() {
		ping.Stop()
		c.close()
	}()

	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := writeFrame(c.conn, opText, msg); err != nil {
				return
			}
		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := writeFrame(c.conn, opPing, nil); err != nil {
				return
			}
		}
	}
}

// clientMessage is what a browser may send us.
type clientMessage struct {
	Type      string `json:"type"`
	Dimension string `json:"dimension"`
}

// readLoop handles control frames and the small set of client commands.
func (c *client) readLoop() {
	defer c.close()
	for {
		// A client that says nothing for this long is presumed gone; the ping
		// from writeLoop keeps healthy connections inside the window.
		_ = c.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		op, payload, err := readFrame(c.conn)
		if err != nil {
			return
		}
		switch op {
		case opClose:
			_ = writeFrame(c.conn, opClose, nil)
			return
		case opPing:
			if err := writeFrame(c.conn, opPong, payload); err != nil {
				return
			}
		case opText:
			var msg clientMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				continue
			}
			if msg.Type == "subscribe" {
				c.mu.Lock()
				c.dimension = msg.Dimension
				c.mu.Unlock()
			}
		}
	}
}

// maxFramePayload bounds what a client may send, so a hostile peer cannot make
// the server allocate arbitrarily.
const maxFramePayload = 1 << 20

// readFrame reads one WebSocket frame, unmasking the payload.
func readFrame(conn net.Conn) (opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(conn, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(conn, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length > maxFramePayload {
		return 0, nil, errors.New("websocket frame too large")
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(conn, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

// writeFrame writes one unmasked server frame.
func writeFrame(conn net.Conn, opcode byte, payload []byte) error {
	n := len(payload)
	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode) // FIN set; this server never fragments

	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, 127)
		header = append(header, ext[:]...)
	}

	if _, err := conn.Write(header); err != nil {
		return err
	}
	if n > 0 {
		if _, err := conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Stats reports hub counters.
type Stats struct {
	Connections int64 `json:"connections"`
	Sent        int64 `json:"messagesSent"`
	Dropped     int64 `json:"clientsDropped"`
}

// Stats snapshots the hub.
func (h *Hub) Stats() Stats {
	return Stats{
		Connections: h.connections.Load(),
		Sent:        h.sent.Load(),
		Dropped:     h.dropped.Load(),
	}
}
