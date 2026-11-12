package ledger

import (
	"sync"
	"time"
)

// EventType distinguishes ledger events.
type EventType string

// Event types.
const (
	// EventPhaseChanged: a request changed phase (From is empty on Enqueue).
	EventPhaseChanged EventType = "PhaseChanged"
	// EventGrantable: a Pending request became grantable (requeue nudge,
	// dispatcher wake-up). Emitted once per transition.
	EventGrantable EventType = "Grantable"
)

// Event is a change notification.
type Event struct {
	Type   EventType
	ID     RequestID
	Kind   Kind
	VM     VMName
	From   Phase
	To     Phase
	Reason Reason
	At     time.Time
}

// subscriber buffers events without bound so the ledger never blocks on a
// slow reader; a goroutine hands them to the channel in order.
type subscriber struct {
	ch     chan Event
	mu     sync.Mutex
	queue  []Event
	notify chan struct{}
	done   chan struct{}
}

func (s *subscriber) push(e Event) {
	s.mu.Lock()
	s.queue = append(s.queue, e)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *subscriber) run() {
	defer close(s.ch)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.queue = nil
			s.mu.Unlock()
			select {
			case <-s.notify:
				continue
			case <-s.done:
				return
			}
		}
		e := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		select {
		case s.ch <- e:
		case <-s.done:
			return
		}
	}
}

// Subscribe returns a channel of events from now on and a cancel function.
// The channel is closed after cancel; events are never dropped while
// subscribed.
func (l *Ledger) Subscribe() (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event), notify: make(chan struct{}, 1), done: make(chan struct{})}
	l.mu.Lock()
	l.subSeq++
	id := l.subSeq
	l.subs[id] = s
	l.mu.Unlock()
	go s.run()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subs, id)
			l.mu.Unlock()
			close(s.done)
		})
	}
}

// emit delivers e to every subscriber; called with l.mu held, never blocks.
func (l *Ledger) emit(e Event) {
	for _, s := range l.subs {
		s.push(e)
	}
}
