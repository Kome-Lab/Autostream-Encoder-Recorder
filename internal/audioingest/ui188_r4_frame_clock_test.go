package audioingest

import (
	"testing"
	"time"
)

func TestUI188FrameClockConsumesCoalescedDeadlinesWithoutChangingSampleSteps(t *testing.T) {
	start := time.Unix(0, 0)
	var emitted uint64
	var sequence uint16
	var timestamp uint32
	for _, elapsed := range []time.Duration{20 * time.Millisecond, 420 * time.Millisecond, 430 * time.Millisecond, time.Second} {
		now := start.Add(elapsed)
		for due := audioFramesDue(start, now, emitted); due > 0; due = audioFramesDue(start, now, emitted) {
			batch := min(due, uint64(8))
			if batch > 8 {
				t.Fatal("unbounded catch-up")
			}
			for range batch {
				emitted++
				sequence++
				timestamp += audioRTPClockStep
			}
		}
		want := uint64(elapsed / audioRTPFrameInterval)
		if emitted != want || uint64(sequence) != want || uint64(timestamp) != want*960 {
			t.Fatalf("lost deadline/sample step at %v: frames=%d seq=%d RTP=%d", elapsed, emitted, sequence, timestamp)
		}
	}
	if audioFramesDue(start, start.Add(-time.Second), 0) != 0 || audioFramesDue(start, start.Add(time.Second), 51) != 0 {
		t.Fatal("clock manufactured future frames")
	}
}
