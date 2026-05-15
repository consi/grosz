package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
)

// SSEBroker manages Server-Sent Events connections.
type SSEBroker struct {
	mu      sync.RWMutex
	clients map[chan string]struct{}
	log     *slog.Logger
	closed  bool
	bootID  string
}

// NewSSEBroker creates a new SSE broker.
func NewSSEBroker(log *slog.Logger) *SSEBroker {
	return &SSEBroker{
		clients: make(map[chan string]struct{}),
		log:     log.With("component", "sse"),
	}
}

// SetBootID configures the per-process boot ID, pushed to each new SSE client on connect.
func (b *SSEBroker) SetBootID(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bootID = id
}

// ServeHTTP handles SSE client connections.
func (b *SSEBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 64)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.clients, ch)
		b.mu.Unlock()
	}()

	// Send initial ping
	_, _ = fmt.Fprintf(w, "event: ping\ndata: connected\n\n")

	// Send boot ID so the frontend can detect a new server build.
	b.mu.RLock()
	bootID := b.bootID
	b.mu.RUnlock()
	if bootID != "" {
		_, _ = fmt.Fprintf(w, "event: bootid\ndata: %s\n\n", bootID)
	}

	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprint(w, msg)
			flusher.Flush()
		}
	}
}

// Broadcast sends an event to all connected SSE clients.
func (b *SSEBroker) Broadcast(event, data string) {
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)

	b.mu.RLock()
	defer b.mu.RUnlock()

	for ch := range b.clients {
		select {
		case ch <- msg:
		default:
			// Client too slow, drop message
		}
	}
}

// Close shuts down the broker and all client channels.
func (b *SSEBroker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.clients {
		close(ch)
		delete(b.clients, ch)
	}
}
