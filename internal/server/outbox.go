package server

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"sync"
)

// M is the body of a JSON message to a browser.
type M map[string]any

// encodeMessage builds one JSON message.
func encodeMessage(msg string, data M) string {
	if data == nil {
		data = M{}
	}
	data["msg"] = msg
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		log.Printf("Could not encode '%s' message: %v", msg, err)
		return `{"msg":"` + msg + `"}`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// frame wraps messages in the envelope the browser expects. The browser
// handles the messages of one frame together.
func frame(msgs ...string) string { return `{"msgs":[` + strings.Join(msgs, ",") + `]}` }

// orNull turns an empty string into JSON null.
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Outbox is the sending side of a browser connection. Any goroutine may send;
// frames go out in the order they were accepted, and sending never blocks: a
// browser that falls too far behind is disconnected.
//
// A connection is in one place at a time - the lobby, or one game. Each move
// starts a new epoch, and whoever serves a place sends with the epoch it was
// given, so that anything it sends after the connection has moved on is
// dropped instead of arriving in the wrong place.
type Outbox struct {
	mu     sync.Mutex
	frames chan string // read by the connection's writer; closed to end it
	closed bool
	epoch  uint64
	abort  func() // drops the connection at once
}

func newOutbox(abort func()) *Outbox {
	return &Outbox{frames: make(chan string, 512), abort: abort}
}

func (o *Outbox) send(scoped bool, epoch uint64, frame string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || (scoped && epoch != o.epoch) {
		return false
	}
	select {
	case o.frames <- frame:
		return true
	default:
		o.closed = true
		close(o.frames)
		o.abort()
		return false
	}
}

// Send queues a frame regardless of where the connection is.
func (o *Outbox) Send(frame string) bool { return o.send(false, 0, frame) }

// SendAt queues a frame if the connection is still in the given epoch.
func (o *Outbox) SendAt(epoch uint64, frame string) bool { return o.send(true, epoch, frame) }

// Advance moves the connection to a new place and returns its epoch.
func (o *Outbox) Advance() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.epoch++
	return o.epoch
}

// Close ends the connection once everything already queued has been sent.
func (o *Outbox) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		close(o.frames)
	}
}
