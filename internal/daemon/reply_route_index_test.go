package daemon

import "testing"

func TestReplyRouteIndex_PutGet(t *testing.T) {
	idx := NewReplyRouteIndex(3)
	idx.Put("m1", "route-A")
	if got := idx.Get("m1"); got != "route-A" {
		t.Fatalf("Get(m1) = %q", got)
	}
	if got := idx.Get("missing"); got != "" {
		t.Fatalf("Get(missing) = %q, want empty", got)
	}
}

func TestReplyRouteIndex_BoundedEvictsOldest(t *testing.T) {
	idx := NewReplyRouteIndex(2)
	idx.Put("m1", "r1")
	idx.Put("m2", "r2")
	idx.Put("m3", "r3") // evicts m1
	if got := idx.Get("m1"); got != "" {
		t.Fatalf("m1 should be evicted, got %q", got)
	}
	if idx.Get("m2") != "r2" || idx.Get("m3") != "r3" {
		t.Fatal("m2/m3 should survive")
	}
}

func TestReplyRouteIndex_NilAndEmptySafe(t *testing.T) {
	var idx *ReplyRouteIndex
	idx.Put("m", "r")
	if idx.Get("m") != "" {
		t.Fatal("nil index Get should be empty")
	}
	idx2 := NewReplyRouteIndex(2)
	idx2.Put("", "r")
	idx2.Put("m", "")
	if idx2.Get("m") != "" {
		t.Fatal("empty-route Put should be ignored")
	}
}
