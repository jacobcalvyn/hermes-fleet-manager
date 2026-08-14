package events

import (
	"sync"
	"time"
)

// Event identifies an authoritative control-plane state transition.
type Event struct {
	StreamID   string    `json:"stream_id"`
	Revision   uint64    `json:"revision"`
	Type       string    `json:"type"`
	ResourceID string    `json:"resource_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Hub broadcasts monotonic revisions to connected clients. Slow clients only
// need the newest revision because they re-read authoritative API state.
type Hub struct {
	mu          sync.Mutex
	streamID    string
	revision    uint64
	nextID      uint64
	subscribers map[uint64]chan Event
}

func New(streamID string) *Hub {
	return &Hub{streamID: streamID, subscribers: make(map[uint64]chan Event)}
}

func (h *Hub) Snapshot() (string, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streamID, h.revision
}

func (h *Hub) Publish(eventType, resourceID string) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revision++
	event := Event{
		StreamID: h.streamID, Revision: h.revision, Type: eventType,
		ResourceID: resourceID, OccurredAt: time.Now().UTC(),
	}
	for _, subscriber := range h.subscribers {
		select {
		case subscriber <- event:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- event:
			default:
			}
		}
	}
	return event
}

func (h *Hub) Subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	updates := make(chan Event, 1)
	h.subscribers[id] = updates
	h.mu.Unlock()
	return updates, func() {
		h.mu.Lock()
		if subscriber, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(subscriber)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}
