package tuikit

import (
	"reflect"
	"strings"
	"testing"
)

func TestWidthIgnoresSGR(t *testing.T) {
	cases := map[string]int{
		"":                        0,
		"abc":                     3,
		Red + "abc" + Reset:       3,
		Bold + "a" + Reset + "bc": 3,
		"┌─┐":                     3,
	}
	for in, want := range cases {
		if got := Width(in); got != want {
			t.Errorf("Width(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestPadAndTruncate(t *testing.T) {
	if got := Pad("ab", 5); got != "ab   " {
		t.Errorf("Pad = %q", got)
	}
	if got := Pad(Red+"ab"+Reset, 4); Width(got) != 4 || !strings.HasPrefix(got, Red) {
		t.Errorf("Pad with SGR = %q (width %d)", got, Width(got))
	}
	if got := Truncate("abcdefgh", 5); got != "ab..." {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("abc", 5); got != "abc" {
		t.Errorf("Truncate short = %q", got)
	}
	if got := Truncate(Green+"abcdefgh"+Reset, 5); Width(got) != 5 || !strings.HasSuffix(got, Reset) {
		t.Errorf("Truncate with SGR = %q (width %d); the colour must be closed", got, Width(got))
	}
	if got := Pad("abcdefgh", 5); got != "ab..." {
		t.Errorf("Pad truncates = %q", got)
	}
}

func TestRightAndCenter(t *testing.T) {
	if got := Right("ab", 5); got != "   ab" {
		t.Errorf("Right = %q", got)
	}
	if got := Center("ab", 6); got != "  ab  " {
		t.Errorf("Center = %q", got)
	}
	if got := Columns("left", "right", 12); got != "left   right" {
		t.Errorf("Columns = %q", got)
	}
}

func TestFrameFillsTheTerminalExactly(t *testing.T) {
	f := Frame{
		Header: "EXPANSE", HeaderRight: "Step 1 of 6", Title: "Welcome",
		Body: []string{"hello", Rule("Disks"), "world"}, Footer: "ENTER continue",
	}
	for _, sz := range []Size{{80, 25}, {160, 50}, {40, 10}} {
		lines := f.Render(sz, Unicode)
		if len(lines) != sz.Rows {
			t.Fatalf("%v: %d lines, want %d", sz, len(lines), sz.Rows)
		}
		for i, l := range lines {
			if Width(l) > sz.Cols {
				t.Errorf("%v line %d is %d wide: %q", sz, i, Width(l), l)
			}
		}
		top := indexOf(lines, "┌")
		if h := lines[top-1]; !strings.Contains(h, "EXPANSE") || !strings.Contains(h, "Step 1 of 6") {
			t.Errorf("%v header above the box: %q", sz, h)
		}
		if ft := lines[indexOf(lines, "└")+1]; !strings.Contains(ft, "ENTER continue") {
			t.Errorf("%v footer below the box: %q", sz, ft)
		}
		joined := Strip(strings.Join(lines, "\n"))
		for _, want := range []string{"Welcome", "hello", "world", "├─ Disks ", "┌", "┘"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%v: missing %q in\n%s", sz, want, joined)
			}
		}
	}
}

func indexOf(lines []string, mark string) int {
	for i, l := range lines {
		if strings.Contains(l, mark) {
			return i
		}
	}
	return -1
}

func TestFrameBoxIsCappedAndCentredOnLargeTerminals(t *testing.T) {
	f := Frame{Title: "T", Body: []string{"x"}}
	lines := f.Render(Size{160, 50}, Unicode)
	top, bottom := indexOf(lines, "┌"), indexOf(lines, "└")
	if top < 3 || bottom-top+1 != MinBoxHeight {
		t.Fatalf("box rows %d..%d; want a centred box of %d rows for a short body", top, bottom, MinBoxHeight)
	}
	lead := strings.Index(lines[top], "┌")
	if lead < 20 || Width(strings.TrimSpace(lines[top])) > MaxBoxWidth {
		t.Fatalf("box row %q: not centred/capped (lead %d)", lines[top], lead)
	}
	long := Frame{Body: make([]string, 100)}
	lines = long.Render(Size{160, 50}, Unicode)
	if top, bottom := indexOf(lines, "┌"), indexOf(lines, "└"); bottom-top+1 != MaxBoxHeight {
		t.Fatalf("a long body should get the largest box (%d rows), got %d", MaxBoxHeight, bottom-top+1)
	}
}

func TestFrameBodyIsClippedToTheBox(t *testing.T) {
	var body []string
	for i := 0; i < 100; i++ {
		body = append(body, strings.Repeat("x", 200))
	}
	f := Frame{Body: body}
	lines := f.Render(Size{80, 25}, ASCII)
	if len(lines) != 25 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if Width(l) > 80 {
			t.Fatalf("line too wide: %q", l)
		}
	}
	if f.Inner(Size{80, 25}) != (Size{76, 21}) {
		t.Fatalf("inner = %v", f.Inner(Size{80, 25}))
	}
}

func TestASCIIGlyphsHaveNoBoxDrawing(t *testing.T) {
	f := Frame{Title: "T", Body: []string{Rule("S")}}
	for _, l := range f.Render(Size{80, 25}, ASCII) {
		for _, r := range l {
			if r > 0x7e {
				t.Fatalf("non-ASCII rune %q in %q", r, l)
			}
		}
	}
}

func TestTable(t *testing.T) {
	rows := [][]string{{"/dev/vda", "32G", "QEMU HARDDISK with a very long model name that overflows"}}
	lines := Table([]string{"DEVICE", "SIZE", "MODEL"}, rows, 40)
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if Width(l) > 40 {
			t.Errorf("row too wide: %q", l)
		}
	}
	if !strings.HasPrefix(Strip(lines[0]), "DEVICE    SIZE  MODEL") {
		t.Errorf("header %q", lines[0])
	}
}

func TestKV(t *testing.T) {
	if got := KV("Hostname", "n1", 10); got != "Hostname   n1" {
		t.Errorf("KV = %q", got)
	}
}

func TestParseKeys(t *testing.T) {
	cases := map[string][]string{
		"\r":                          {"enter"},
		"\r\n":                        {"enter"},
		"\x1b[A\x1b[B":                {"up", "down"},
		"\x7f":                        {"backspace"},
		"\x03":                        {"ctrl-c"},
		" ":                           {" "},
		"q":                           {"q"},
		"ssh-ed25519 AAAAq me@host\n": {"ssh-ed25519 AAAAq me@host", "enter"},
		"\x1b[C":                      nil, // unhandled escape sequences are dropped whole
	}
	for in, want := range cases {
		if got := ParseKeys([]byte(in)); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseKeys(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProgressBar(t *testing.T) {
	bar := ProgressBar(3, 9, 20, ASCII)
	if Width(bar) != 20 || !strings.HasPrefix(bar, "[") || !strings.HasSuffix(bar, "]") {
		t.Fatalf("bar = %q", bar)
	}
	if strings.Count(bar, "#") != 6 {
		t.Fatalf("bar %q: want 6 of 18 cells filled", bar)
	}
	if Width(ProgressBar(0, 0, 10, ASCII)) != 10 {
		t.Fatal("empty total must not divide by zero")
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("the quick brown fox jumps over the lazy dog", 10)
	want := []string{"the quick", "brown fox", "jumps over", "the lazy", "dog"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Wrap = %q", got)
	}
	if got := Wrap("", 10); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("Wrap empty = %q", got)
	}
}
