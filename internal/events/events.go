// Package events is a small fan-out hub so the engine, clipboard watcher and
// frontends (CLI, web UI) can talk without knowing about each other.
package events

import "sync"

type Type string

const (
	URLDetected   Type = "url_detected"   // Data: URLDetectedData
	JobUpdated    Type = "job_updated"    // Data: engine.Job
	ToolsStatus   Type = "tools_status"   // Data: tools.Status
	LinksProgress Type = "links_progress" // Data: engine.LinksProgress
	SignInStatus  Type = "signin_status"  // Data: engine.SignInStatus
)

type URLDetectedData struct {
	URL string `json:"url"`
}

type Event struct {
	Type Type `json:"type"`
	Data any  `json:"data"`
}

type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a channel of events and a function that unsubscribes.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish never blocks: a subscriber that falls this far behind misses events.
// Every event carries full state (not deltas), so the next one catches it up.
func (h *Hub) Publish(t Type, data any) {
	e := Event{Type: t, Data: data}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
