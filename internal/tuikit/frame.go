package tuikit

import "strings"

// Size is a terminal's dimensions in cells.
type Size struct{ Cols, Rows int }

// VT is the Linux virtual console's minimum, and the layout's design size.
var VT = Size{Cols: 80, Rows: 25}

// Glyphs are the box-drawing characters; ASCII is the fallback for terminals without them.
type Glyphs struct {
	TL, TR, BL, BR, H, V, LT, RT string
	Fill, Empty                  string // progress bar cells
}

var (
	Unicode = Glyphs{"┌", "┐", "└", "┘", "─", "│", "├", "┤", "#", "-"}
	ASCII   = Glyphs{"+", "+", "+", "+", "-", "|", "+", "+", "#", "-"}
)

// The box fills the VT; on larger terminals it is capped, sized to its content (never
// below the VT's box height) and centred, with the header and footer hugging it.
const (
	MaxBoxWidth  = 100
	MaxBoxHeight = 40
	MinBoxHeight = 23
)

const rulePrefix = "\x00rule:"

// Rule is a body line that Frame draws as a titled divider ("├─ Title ────┤").
func Rule(title string) string { return rulePrefix + title }

// Frame is one full-screen page: a reverse-video header bar, a titled box holding
// Body (clipped to the box), and a footer line of key hints. Callers size Body with
// Inner, which is the largest box the terminal allows.
type Frame struct {
	Header, HeaderRight string
	Title               string
	Body                []string
	Footer              string
}

// box is where the frame's box lands for a terminal size.
type box struct{ x, y, w, h int }

func (f Frame) box(sz Size) box {
	w := min(sz.Cols, MaxBoxWidth)
	avail := min(max(sz.Rows-2, 3), MaxBoxHeight)
	h := avail
	if f.Body != nil {
		h = min(avail, max(len(f.Body)+2, MinBoxHeight))
	}
	return box{x: (sz.Cols - w) / 2, y: 1 + (sz.Rows-2-h)/2, w: w, h: h}
}

// Inner is the most space Body lines can have at sz: the largest box minus border and margins.
func (f Frame) Inner(sz Size) Size {
	b := Frame{}.box(sz)
	return Size{Cols: b.w - 4, Rows: b.h - 2}
}

// Render draws the frame as exactly sz.Rows lines, none wider than sz.Cols.
func (f Frame) Render(sz Size, g Glyphs) []string {
	sz = Size{Cols: max(sz.Cols, 20), Rows: max(sz.Rows, 5)} // a tiny terminal scrolls rather than panics
	b := f.box(sz)
	lines := make([]string, sz.Rows)
	margin := strings.Repeat(" ", b.x)
	inner := Size{Cols: b.w - 4, Rows: b.h - 2}
	lines[b.y-1] = margin + Reverse + " " + Columns(Bold+f.Header, f.HeaderRight, b.w-2) + " " + Reset
	lines[b.y] = margin + f.edge(g.TL, g.TR, f.Title, b.w, g)
	for i := 0; i < inner.Rows; i++ {
		body := ""
		if i < len(f.Body) {
			body = f.Body[i]
		}
		if strings.HasPrefix(body, rulePrefix) {
			lines[b.y+1+i] = margin + f.edge(g.LT, g.RT, strings.TrimPrefix(body, rulePrefix), b.w, g)
			continue
		}
		lines[b.y+1+i] = margin + g.V + " " + Pad(body, inner.Cols) + " " + g.V
	}
	lines[b.y+b.h-1] = margin + f.edge(g.BL, g.BR, "", b.w, g)
	lines[b.y+b.h] = margin + " " + Truncate(f.Footer, b.w-1)
	return lines
}

// edge draws a horizontal border of w cells with an optional title after the corner.
func (f Frame) edge(left, right, title string, w int, g Glyphs) string {
	fill := w - 2
	if title != "" {
		title = g.H + " " + Bold + Truncate(title, max(fill-4, 0)) + Reset + " "
		fill -= Width(title)
	}
	return left + title + strings.Repeat(g.H, max(fill, 0)) + right
}

// Keys formats key hints for a footer: "ENTER continue   q quit" with the keys bold.
func Keys(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, Bold+Cyan+pairs[i]+Reset+" "+pairs[i+1])
	}
	return strings.Join(parts, "   ")
}
