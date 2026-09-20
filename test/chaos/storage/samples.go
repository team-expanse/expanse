package chaosstorage

import (
	"sort"
	"time"
)

// Sample is one client write: when it started (since the run began), how
// long it took, and how it ended.
type Sample struct {
	At  time.Duration
	Dur time.Duration
	Err error
}

// Samples is a set of write outcomes.
type Samples []Sample

// Percentile is the nearest-rank latency at p in (0,1]; 0 if empty.
func (s Samples) Percentile(p float64) time.Duration {
	if len(s) == 0 {
		return 0
	}
	d := make([]time.Duration, len(s))
	for i, x := range s {
		d[i] = x.Dur
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	rank := int(p*float64(len(d))+0.999999999) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(d) {
		rank = len(d) - 1
	}
	return d[rank]
}

// Window keeps the samples that started in [from, to).
func (s Samples) Window(from, to time.Duration) Samples {
	var out Samples
	for _, x := range s {
		if x.At >= from && x.At < to {
			out = append(out, x)
		}
	}
	return out
}

// Errors keeps the failed writes.
func (s Samples) Errors() Samples {
	var out Samples
	for _, x := range s {
		if x.Err != nil {
			out = append(out, x)
		}
	}
	return out
}

// LongestStall is the widest gap between consecutive acked-write
// completions, i.e. the longest stretch the volume made no progress.
func (s Samples) LongestStall() time.Duration {
	var ends []time.Duration
	for _, x := range s {
		if x.Err == nil {
			ends = append(ends, x.At+x.Dur)
		}
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })
	var worst time.Duration
	for i := 1; i < len(ends); i++ {
		if g := ends[i] - ends[i-1]; g > worst {
			worst = g
		}
	}
	return worst
}
