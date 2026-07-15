package koe

import "testing"

func TestMicPreRollKeepsLatestFramesAndCopiesInput(t *testing.T) {
	r := newMicPreRoll(2)
	a := []int16{1}
	r.Push(a)
	a[0] = 99 // the ring must not alias a capture backend's reusable frame
	r.Push([]int16{2})
	r.Push([]int16{3})
	got := r.Drain([]int16{4})
	if len(got) != 3 || got[0][0] != 2 || got[1][0] != 3 || got[2][0] != 4 {
		t.Fatalf("drain = %v, want latest pre-roll [2 3] then current [4]", got)
	}
	if again := r.Drain([]int16{5}); len(again) != 1 || again[0][0] != 5 {
		t.Fatalf("drain should clear ring, got %v", again)
	}
}

func TestMicPreRollCanBeDisabled(t *testing.T) {
	r := newMicPreRoll(0)
	r.Push([]int16{1})
	got := r.Drain([]int16{2})
	if len(got) != 1 || got[0][0] != 2 {
		t.Fatalf("disabled pre-roll = %v", got)
	}
}
