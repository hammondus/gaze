package main

import (
	"testing"

	"github.com/hammondus/gaze/internal/report"
)

// TestRingBounds pins the two properties the buffer exists for: it never
// exceeds its capacity, and when full it loses the oldest, not the newest.
func TestRingBounds(t *testing.T) {
	var r ring
	for i := 0; i < ringCap+10; i++ {
		r.push(report.Report{Generation: i})
	}
	if r.len() != ringCap {
		t.Fatalf("ring holds %d, capacity is %d", r.len(), ringCap)
	}
	oldest := r.peek(1)
	if oldest[0].Generation != 10 {
		t.Errorf("oldest survivor is %d, want 10: the ring must drop from the front", oldest[0].Generation)
	}
}

// TestRingPeekAndDrop covers the send-then-confirm shape: peek does not
// remove, so a failed post loses nothing.
func TestRingPeekAndDrop(t *testing.T) {
	var r ring
	for i := 0; i < 5; i++ {
		r.push(report.Report{Generation: i})
	}
	batch := r.peek(3)
	if len(batch) != 3 || batch[0].Generation != 0 || batch[2].Generation != 2 {
		t.Fatalf("peek(3) = %+v, want generations 0..2", batch)
	}
	if r.len() != 5 {
		t.Errorf("peek removed reports: %d left, want 5", r.len())
	}
	r.drop(3)
	if r.len() != 2 || r.peek(1)[0].Generation != 3 {
		t.Errorf("after drop(3): %d left, oldest %d; want 2 left, oldest 3", r.len(), r.peek(1)[0].Generation)
	}
	r.drop(10)
	if r.len() != 0 {
		t.Errorf("drop past the end left %d", r.len())
	}
}
