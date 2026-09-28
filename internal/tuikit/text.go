// Package tuikit is the small rendering kit shared by the installer TUI and the node
// console: 16-colour SGR styles, width-aware padding, a framed full-screen layout and
// a raw-mode terminal driver. Render functions are pure; only Term touches the tty.
package tuikit

import (
	"strings"
	"unicode/utf8"
)

// SGR sequences. The palette is the Linux VT's 16 colours, used with restraint:
// cyan is the accent, red is destructive, green is good, yellow is a warning.
const (
	Reset   = "\x1b[0m"
	Bold    = "\x1b[1m"
	Dim     = "\x1b[2m"
	Reverse = "\x1b[7m"
	Red     = "\x1b[31m"
	Green   = "\x1b[32m"
	Yellow  = "\x1b[33m"
	Cyan    = "\x1b[36m"
	White   = "\x1b[37m"
)

const ellipsis = "..."

// Width is the visible width of s: SGR escape sequences take no cells.
func Width(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipSGR(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// Strip removes SGR sequences, leaving the visible text.
func Strip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipSGR(s, i)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// skipSGR returns the index just past the CSI sequence starting at s[i].
func skipSGR(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
	}
	return min(j+1, len(s))
}

// Truncate cuts s to at most w cells, ending in "..." and closing any open colour.
func Truncate(s string, w int) string {
	if Width(s) <= w {
		return s
	}
	keep := max(w-len(ellipsis), 0)
	var b strings.Builder
	n, styled := 0, false
	for i := 0; i < len(s) && n < keep; {
		if s[i] == 0x1b {
			j := skipSGR(s, i)
			b.WriteString(s[i:j])
			styled = true
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		i += size
		n++
	}
	b.WriteString(ellipsis[:min(len(ellipsis), w)])
	if styled {
		b.WriteString(Reset)
	}
	return b.String()
}

// Pad fits s into exactly w cells: left-aligned, space-filled, truncated if longer.
func Pad(s string, w int) string {
	s = Truncate(s, w)
	return s + strings.Repeat(" ", w-Width(s))
}

// Right right-aligns s in w cells.
func Right(s string, w int) string {
	s = Truncate(s, w)
	return strings.Repeat(" ", w-Width(s)) + s
}

// Center centres s in w cells.
func Center(s string, w int) string {
	s = Truncate(s, w)
	left := (w - Width(s)) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", w-Width(s)-left)
}

// Columns puts left and right at the two ends of a w-cell line.
func Columns(left, right string, w int) string {
	right = Truncate(right, w)
	left = Pad(left, max(w-Width(right)-1, 0))
	return left + " " + right
}

// KV is a "label  value" line with the label padded to labelWidth.
func KV(label, value string, labelWidth int) string {
	return Pad(label, labelWidth) + " " + value
}

// Wrap breaks s into lines of at most w cells on spaces (a word longer than w is cut).
func Wrap(s string, w int) []string {
	lines := []string{""}
	for _, word := range strings.Fields(s) {
		for Width(word) > w {
			if lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			lines[len(lines)-1] = word[:w]
			word = word[w:]
			lines = append(lines, "")
		}
		cur := lines[len(lines)-1]
		switch {
		case cur == "":
			lines[len(lines)-1] = word
		case Width(cur)+1+Width(word) <= w:
			lines[len(lines)-1] = cur + " " + word
		default:
			lines = append(lines, word)
		}
	}
	return lines
}

// Table lays out rows under headers in w cells: columns are as wide as their widest
// cell (two-space gutters) and the last column absorbs the rest.
func Table(headers []string, rows [][]string, w int) []string {
	widths := make([]int, len(headers))
	for _, r := range append([][]string{headers}, rows...) {
		for i, c := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], Width(c))
			}
		}
	}
	line := func(cells []string) string {
		var b strings.Builder
		for i, wd := range widths {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			if i == len(widths)-1 {
				b.WriteString(Truncate(c, max(w-Width(b.String()), 0)))
				break
			}
			b.WriteString(Pad(c, wd) + "  ")
		}
		return Truncate(b.String(), w)
	}
	out := []string{Bold + line(headers) + Reset}
	for _, r := range rows {
		out = append(out, line(r))
	}
	return out
}

// ProgressBar draws "[####....]" for done of total in exactly w cells.
func ProgressBar(done, total, w int, g Glyphs) string {
	inner := max(w-2, 0)
	filled := 0
	if total > 0 {
		filled = min(inner, inner*done/total)
	}
	return "[" + strings.Repeat(g.Fill, filled) + strings.Repeat(g.Empty, inner-filled) + "]"
}
