package koe

import "testing"

func TestScalePCMForBargeDuck(t *testing.T) {
	got := scalePCM([]int16{-1000, 0, 1000}, defaultBargeDuckGain)
	want := []int16{-50, 0, 50}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scalePCM[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestScalePCMClampsGain(t *testing.T) {
	input := []int16{-1000, 1000}
	if got := scalePCM(input, 2); &got[0] != &input[0] {
		t.Fatal("gain above one should clamp to unity without copying")
	}
	got := scalePCM(input, -1)
	if got[0] != 0 || got[1] != 0 {
		t.Fatalf("negative gain should clamp to silence, got %v", got)
	}
}
