package hub

import "sync"

type EventHub struct {
	mu   sync.Mutex
	subs map[int]chan map[string]any
	next int
	last map[string]any
}

func NewHub() *EventHub {
	return &EventHub{subs: map[int]chan map[string]any{}, last: map[string]any{}}
}

func (h *EventHub) Subscribe() (int, <-chan map[string]any, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan map[string]any, 16)
	if len(h.last) > 0 {
		ch <- h.copy(h.last)
	}
	h.subs[id] = ch
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			close(c)
			delete(h.subs, id)
		}
	}
	return id, ch, cancel
}

func (h *EventHub) Broadcast(sig map[string]any) {
	h.mu.Lock()
	for k, v := range sig {
		h.last[k] = v
	}
	for _, ch := range h.subs {
		select {
		case ch <- h.copy(sig):
		default:
		}
	}
	h.mu.Unlock()
}

func (h *EventHub) copy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
