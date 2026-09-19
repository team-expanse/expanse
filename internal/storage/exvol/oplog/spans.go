package oplog

const spanPage = 4096

// Span is the byte range one claimed op wrote (Length 0 = flush marker).
type Span struct {
	Offset uint64
	Length uint32
}

// Spans answers "did a later op rewrite this op's range?" for one
// replica's claims, at 4 KiB granularity (conservative: a shared page
// counts as overwritten). Built once; lookups are O(pages in the op).
type Spans struct {
	spans map[uint64]Span
	last  map[uint64]uint64 // page -> highest seq that touched it
}

// NewSpans indexes a replica's claimed op ranges.
func NewSpans(spans map[uint64]Span) *Spans {
	s := &Spans{spans: spans, last: map[uint64]uint64{}}
	for seq, sp := range spans {
		first, end := pages(sp)
		for pg := first; pg < end; pg++ {
			if seq > s.last[pg] {
				s.last[pg] = seq
			}
		}
	}
	return s
}

// Superseded reports whether a later op touched any page of seq's range.
// A nil or unknown span is never superseded (CRC checks stay strict).
func (s *Spans) Superseded(seq uint64) bool {
	if s == nil {
		return false
	}
	sp, ok := s.spans[seq]
	if !ok || sp.Length == 0 {
		return false
	}
	first, end := pages(sp)
	for pg := first; pg < end; pg++ {
		if s.last[pg] > seq {
			return true
		}
	}
	return false
}

// pages returns the half-open page range [first, end) a span touches.
func pages(sp Span) (first, end uint64) {
	if sp.Length == 0 {
		return 0, 0
	}
	return sp.Offset / spanPage, (sp.Offset+uint64(sp.Length)-1)/spanPage + 1
}
