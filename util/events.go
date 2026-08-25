package util

import "sync"

// Event is a lightweight broadcast message.
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// EventBus is kept for backward compatibility. New code should use
// DefaultVersionNotifier directly.
//
// It wraps a VersionNotifier so that PublishVersion/BumpVersion calls share
// the same version table and SSE connection set.
type EventBus struct {
	*VersionNotifier
	mu   sync.RWMutex
	subs map[<-chan Event]chan Event
}

// NewEventBus creates a new EventBus backed by a VersionNotifier.
func NewEventBus() *EventBus {
	return &EventBus{
		VersionNotifier: NewVersionNotifier(),
		subs:            make(map[<-chan Event]chan Event),
	}
}

// DefaultEventBus is the process-wide event bus used historically by the admin
// panel. It now delegates version tracking to DefaultVersionNotifier.
var DefaultEventBus = NewEventBus()

func init() {
	// Share the same version table and connection set so that callers using
	// either DefaultEventBus or DefaultVersionNotifier see consistent state.
	DefaultEventBus.VersionNotifier = DefaultVersionNotifier
}

// Subscribe registers a new consumer. The returned channel is buffered but is
// no longer used for version updates; new code should subscribe via
// VersionNotifier.Subscribe.
func (b *EventBus) Subscribe() <-chan Event {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subs[ch] = ch
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a consumer and closes its channel.
func (b *EventBus) Unsubscribe(recv <-chan Event) {
	b.mu.Lock()
	ch, ok := b.subs[recv]
	if ok {
		delete(b.subs, recv)
	}
	b.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Publish broadcasts an event to all current subscribers without blocking.
func (b *EventBus) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Channel full or consumer gone: drop the event.
		}
	}
}

// PublishVersion increments the version for topic and broadcasts the full
// version vector as a heartbeat. It returns the new version number.
func (b *EventBus) PublishVersion(topic string) uint64 {
	return b.BumpVersion(topic)
}
