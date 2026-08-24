package store

import "sync"

// EventType names the kinds of notification pushed to connected UIs.
type EventType string

const (
	EventCreated EventType = "created"
	EventUpdated EventType = "updated"
	EventDelta   EventType = "delta"
	EventDeleted EventType = "deleted"
)

// Event is one notification. Full bodies never travel here: the detail view
// fetches them over the REST routes instead.
type Event struct {
	Type  EventType `json:"-"`
	ID    string    `json:"id"`
	Entry *Entry    `json:"-"`
	Chunk string    `json:"chunk,omitempty"`
}

// subscriberBuffer sizes each subscriber channel. A subscriber that cannot keep
// up is dropped rather than allowed to block capture.
const subscriberBuffer = 256

// Broker fans events out to every connected subscriber.
type Broker struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	closed bool
}

func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a channel of events and the function that releases it.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() { b.drop(ch) })
	}
}

// Publish delivers an event to every subscriber. A saturated channel means the
// subscriber is too slow: it is disconnected so the capture path never blocks.
func (b *Broker) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

func (b *Broker) drop(ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
}

// Close disconnects every subscriber.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}
