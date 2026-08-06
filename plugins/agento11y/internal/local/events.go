package local

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// changeEvent is the payload broadcast to SSE subscribers when the local
// store changes. It carries identifiers only; the client refetches the
// affected views over the existing JSON APIs. A missed list event
// self-heals on the next event or the client-side 60s backstop poll;
// the backstop refreshes only the list, so a missed event for the
// currently-open detail view only recovers on another targeted event
// or when the user reopens the conversation.
type changeEvent struct {
	ConversationID string `json:"conversation_id,omitempty"`
	GenerationID   string `json:"generation_id,omitempty"`
	// Import carries history-import progress. It is the one event that is a
	// state update rather than a refetch hint: an import writes thousands of
	// generations, and the counters are what the user watches while it runs.
	Import *ImportRun `json:"import,omitempty"`
}

// eventSubBuffer caps each subscriber's pending queue. The broadcaster
// drops on full buffer rather than blocking, so a stalled viewer cannot
// stall handleGenerations. The client refetches the full conversation
// list on every event, so a dropped event during a burst self-heals for
// the list view. The detail view of the dropped event's conversation is
// only refreshed by the next targeted event or by the user reopening
// it; the 60s backstop does not cover detail.
const eventSubBuffer = 32

// defaultEventPingInterval is the heartbeat the SSE handler writes when
// no real events have arrived. Chosen well under typical proxy idle
// timeouts (~30-60s) so half-open connections stay alive and a dead
// daemon is detected. Exposed via s.eventPingInterval so tests can
// shorten it without changing handler semantics.
const defaultEventPingInterval = 25 * time.Second

// eventSub is one viewer's subscription to the change stream.
type eventSub struct {
	ch chan changeEvent
}

// eventHub fans out changeEvents to every connected viewer. It is safe
// for concurrent use; the zero value is not — call newEventHub.
type eventHub struct {
	mu     sync.Mutex
	subs   map[*eventSub]struct{}
	closed bool
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[*eventSub]struct{})}
}

// subscribe registers a new subscriber. Returns nil once closeAll has
// been called so the caller can short-circuit to "stream closed" instead
// of holding a connection open against a dead hub.
func (h *eventHub) subscribe() *eventSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	sub := &eventSub{ch: make(chan changeEvent, eventSubBuffer)}
	h.subs[sub] = struct{}{}
	return sub
}

// unsubscribe removes a subscriber and closes its channel. Safe to call
// after closeAll: the subscriber is no longer registered, so the second
// close is skipped.
func (h *eventHub) unsubscribe(sub *eventSub) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	close(sub.ch)
}

// broadcast non-blocking sends ev to every subscriber. A full buffer
// drops the event for that subscriber. After closeAll the call is a
// no-op so late ingest events do not panic on a closed channel.
func (h *eventHub) broadcast(ev changeEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default:
			// Subscriber is behind. The client refetches the full list
			// on the next event or via the backstop poll, so dropping
			// here keeps handleGenerations responsive without losing
			// list correctness.
		}
	}
}

// closeAll closes every subscriber channel and marks the hub closed so
// subsequent subscribe/broadcast calls are no-ops. Idempotent. Used at
// shutdown so open SSE handlers return promptly instead of holding the
// httpSrv.Shutdown deadline.
func (h *eventHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for sub := range h.subs {
		close(sub.ch)
	}
	h.subs = nil
}

// handleEvents streams changeEvent JSON frames over Server-Sent Events.
// One connection per viewer; the handler returns when the client
// disconnects (r.Context().Done()) or the hub is closed at shutdown.
// The browser EventSource auto-reconnects on transport errors, so we do
// not need to keep a stalled connection alive on the server side.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sub := s.hub.subscribe()
	if sub == nil {
		// Hub already closed (server shutting down). Reply before
		// writing the SSE headers so the client treats it as an
		// ordinary failure and retries when the daemon is back.
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.hub.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tell nginx-style reverse proxies not to buffer the response, or
	// they hold every frame until the response closes.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Initial comment + flush so the browser marks the stream open and
	// any proxy that buffers headers sends them through immediately.
	if _, err := io.WriteString(w, ":\n\n"); err != nil {
		return
	}
	flusher.Flush()

	interval := s.eventPingInterval
	if interval <= 0 {
		interval = defaultEventPingInterval
	}
	ping := time.NewTicker(interval)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.ch:
			if !ok {
				// Hub closed the channel during shutdown.
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				// A marshal failure here is a developer bug; skip the
				// frame so the stream stays usable for the next event.
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := io.WriteString(w, ":\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
