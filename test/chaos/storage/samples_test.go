package chaosstorage

import (
	"errors"
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestPercentile(t *testing.T) {
	var s Samples
	for i := 1; i <= 100; i++ {
		s = append(s, Sample{Dur: ms(i)})
	}
	if got := s.Percentile(0.5); got != ms(50) {
		t.Errorf("p50 = %v, want 50ms", got)
	}
	if got := s.Percentile(0.99); got != ms(99) {
		t.Errorf("p99 = %v, want 99ms", got)
	}
	if got := s.Percentile(1); got != ms(100) {
		t.Errorf("max = %v, want 100ms", got)
	}
	if got := (Samples{}).Percentile(0.99); got != 0 {
		t.Errorf("empty p99 = %v, want 0", got)
	}
}

func TestWindowKeepsSamplesStartedInRange(t *testing.T) {
	s := Samples{{At: ms(10)}, {At: ms(20)}, {At: ms(30)}}
	if got := s.Window(ms(20), ms(30)); len(got) != 1 || got[0].At != ms(20) {
		t.Fatalf("Window = %+v", got)
	}
}

func TestErrorsFiltersFailedWrites(t *testing.T) {
	s := Samples{{}, {Err: errors.New("x")}, {}}
	if got := s.Errors(); len(got) != 1 {
		t.Fatalf("Errors() = %d, want 1", len(got))
	}
}

func TestLongestStallIsWidestGapBetweenAcks(t *testing.T) {
	s := Samples{
		{At: ms(0), Dur: ms(5)},
		{At: ms(10), Dur: ms(5), Err: errors.New("x")}, // does not count as progress
		{At: ms(500), Dur: ms(5)},
	}
	if got := s.LongestStall(); got != ms(500) {
		t.Fatalf("LongestStall = %v, want 500ms", got)
	}
}
