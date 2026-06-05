package daemon

import (
	"container/list"
	"sync"
)

// ReplyRouteIndex maps an inbound message_id to the route_key it was handled
// on, so a later reply_delivery_result frame (which echoes that message_id) can
// be enqueued onto the right route's SystemEventStore. Bounded FIFO — a result
// normally arrives within seconds of the reply, so a small cap suffices.
//
// cap is justified at 256: a burst of concurrent IM replies awaiting their
// platform results; symptom when it binds is an old message_id evicted before
// its (late) result arrives → that one result is dropped (the next-turn binding
// poll still reconciles). Override via the daemon's `agent.reply_route_index_cap`.
type ReplyRouteIndex struct {
	mu    sync.Mutex
	cap   int
	items map[string]*list.Element
	order *list.List // front = oldest
}

type replyRouteEntry struct {
	msgID    string
	routeKey string
}

// NewReplyRouteIndex returns an index bounded to capItems (<=0 → 256).
func NewReplyRouteIndex(capItems int) *ReplyRouteIndex {
	if capItems <= 0 {
		capItems = 256
	}
	return &ReplyRouteIndex{cap: capItems, items: make(map[string]*list.Element), order: list.New()}
}

// Put records msgID → routeKey, evicting the oldest if over cap. No-op on nil
// receiver or empty msgID/routeKey.
func (x *ReplyRouteIndex) Put(msgID, routeKey string) {
	if x == nil || msgID == "" || routeKey == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if el, ok := x.items[msgID]; ok {
		el.Value.(*replyRouteEntry).routeKey = routeKey
		x.order.MoveToBack(el)
		return
	}
	el := x.order.PushBack(&replyRouteEntry{msgID: msgID, routeKey: routeKey})
	x.items[msgID] = el
	for x.order.Len() > x.cap {
		front := x.order.Front()
		if front == nil {
			break
		}
		x.order.Remove(front)
		delete(x.items, front.Value.(*replyRouteEntry).msgID)
	}
}

// Get returns the route_key for msgID, or "" if unknown / nil receiver.
func (x *ReplyRouteIndex) Get(msgID string) string {
	if x == nil || msgID == "" {
		return ""
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if el, ok := x.items[msgID]; ok {
		return el.Value.(*replyRouteEntry).routeKey
	}
	return ""
}
