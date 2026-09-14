// Table tests for quantity parsing (Phase 04 T02).
//
// The forms covered here are exactly those named in PHASE04.md §8
// ("Quantity parsing: 500m, 1.5, 2, 256Mi, 1Gi, 1G, invalid forms") plus
// rule V9's requirement that every error message name the bad value.
package quantity

import (
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/errors"
)

func TestParseCPU(t *testing.T) {
	valid := []struct {
		in   string
		want int64 // millicores
	}{
		{"500m", 500}, // V9 named form
		{"2", 2000},   // V9 named form
		{"1.5", 1500}, // V9 named form
		{"0", 0},
		{"0m", 0},
		{"1", 1000},
		{"10m", 10},
		{"1500m", 1500},
		{"1.234", 1234},
		{"0.9999", 1000}, // rounds half up into the next whole core
		{"1.2345", 1235}, // fourth decimal digit rounds
		{"128", 128000},
		{" 2 ", 2000}, // surrounding whitespace tolerated
	}
	for _, c := range valid {
		got, err := ParseCPU(c.in)
		if err != nil {
			t.Errorf("ParseCPU(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got.Milli != c.want {
			t.Errorf("ParseCPU(%q).Milli = %d, want %d", c.in, got.Milli, c.want)
		}
	}

	invalid := []string{
		"",                       // empty
		"   ",                    // whitespace only
		"abc",                    // not a number
		"-2",                     // negative
		"+2",                     // sign
		"1.5m",                   // fractional millicores
		"500M",                   // wrong suffix (M is not millicores)
		"m",                      // bare suffix
		".",                      // bare dot
		".5",                     // no leading integer part
		"5.",                     // trailing dot
		"1.2.3",                  // two dots
		"1e3",                    // scientific notation
		"0x10",                   // hex
		"2 Gi",                   // a byte quantity, not a CPU quantity
		"9999999999999999999999", // overflow
	}
	for _, in := range invalid {
		_, err := ParseCPU(in)
		if err == nil {
			t.Errorf("ParseCPU(%q): expected error, got nil", in)
			continue
		}
		// V9: the error message must name the bad value.
		if !errors.Is(err, errors.KindInvalid) {
			t.Errorf("ParseCPU(%q): error kind = %q, want invalid", in, errors.KindOf(err))
		}
		trimmed := strings.TrimSpace(in)
		if trimmed != "" && !strings.Contains(err.Error(), trimmed) {
			t.Errorf("ParseCPU(%q): error message %q does not name the bad value", in, err)
		}
	}
}

func TestParseBytes(t *testing.T) {
	valid := []struct {
		in   string
		want int64
	}{
		{"256Mi", 256 * 1024 * 1024}, // V9 named form
		{"1Gi", 1 << 30},             // V9 named form
		{"1G", 1000 * 1000 * 1000},   // §8 named form (decimal)
		{"10Gi", 10 << 30},
		{"0", 0},
		{"512", 512}, // plain bytes
		{"1k", 1000},
		{"1Ki", 1024},
		{"100M", 100 * 1000 * 1000},
		{"1Mi", 1 << 20},
		{"8Ti", 8 << 40},
		{"2Pi", 2 << 50},
		{"1E", 1e18},
		{"1Ei", 1 << 60},
		{" 10Gi ", 10 << 30}, // surrounding whitespace tolerated
	}
	for _, c := range valid {
		got, err := ParseBytes(c.in)
		if err != nil {
			t.Errorf("ParseBytes(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got.N != c.want {
			t.Errorf("ParseBytes(%q).N = %d, want %d", c.in, got.N, c.want)
		}
	}

	invalid := []string{
		"",                     // empty
		"abc",                  // not a number
		"-1",                   // negative
		"1.5",                  // bare float: byte quantities must be whole
		"1.5Gi",                // fractional binary quantity
		"1 Gi",                 // space inside
		"1GiB",                 // unknown suffix
		"1bi",                  // unknown suffix
		"Gi",                   // bare suffix
		"1kb",                  // lowercase unknown suffix
		"1x",                   // unknown suffix
		"1m",                   // millicores are not bytes
		"0x10",                 // hex
		"9223372036854775808",  // plain overflow
		"10Ei",                 // 10 * 2^60 overflows int64
		"18446744073709551616", // 2^64
	}
	for _, in := range invalid {
		_, err := ParseBytes(in)
		if err == nil {
			t.Errorf("ParseBytes(%q): expected error, got nil", in)
			continue
		}
		if !errors.Is(err, errors.KindInvalid) {
			t.Errorf("ParseBytes(%q): error kind = %q, want invalid", in, errors.KindOf(err))
		}
		trimmed := strings.TrimSpace(in)
		if trimmed != "" && !strings.Contains(err.Error(), trimmed) {
			t.Errorf("ParseBytes(%q): error message %q does not name the bad value", in, err)
		}
	}
}

func TestCPUString(t *testing.T) {
	cases := []struct {
		milli int64
		want  string
	}{
		{500, "500m"},
		{2000, "2"},
		{1500, "1500m"},
		{0, "0"},
	}
	for _, c := range cases {
		got := CPU{Milli: c.milli}.String()
		if got != c.want {
			t.Errorf("CPU{Milli:%d}.String() = %q, want %q", c.milli, got, c.want)
		}
		// Canonical forms round-trip exactly.
		re, err := ParseCPU(got)
		if err != nil {
			t.Errorf("ParseCPU(%q): unexpected error: %v", got, err)
			continue
		}
		if re.Milli != c.milli {
			t.Errorf("canonical %q re-parsed to %d, want %d", got, re.Milli, c.milli)
		}
	}
}

func TestBytesString(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{256 * 1024 * 1024, "256Mi"},
		{1 << 30, "1Gi"},
		{1000 * 1000 * 1000, "1G"},
		{1024, "1Ki"},
		{1000, "1k"},
		{512, "512"},
		{1500, "1500"},
		{0, "0"},
	}
	for _, c := range cases {
		got := Bytes{N: c.n}.String()
		if got != c.want {
			t.Errorf("Bytes{N:%d}.String() = %q, want %q", c.n, got, c.want)
		}
		// Canonical forms round-trip exactly.
		re, err := ParseBytes(got)
		if err != nil {
			t.Errorf("ParseBytes(%q): unexpected error: %v", got, err)
			continue
		}
		if re.N != c.n {
			t.Errorf("canonical %q re-parsed to %d, want %d", got, re.N, c.n)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b            CPU
		wantGTE, wantLT bool
	}{
		{CPU{Milli: 2000}, CPU{Milli: 500}, true, false},  // limits 2 >= requests 500m
		{CPU{Milli: 500}, CPU{Milli: 2000}, false, true},  // V8 violation direction
		{CPU{Milli: 1000}, CPU{Milli: 1000}, true, false}, // equal is fine
	}
	for _, c := range cases {
		if got := c.a.GTE(c.b); got != c.wantGTE {
			t.Errorf("%v.GTE(%v) = %v, want %v", c.a, c.b, got, c.wantGTE)
		}
		if got := c.a.LT(c.b); got != c.wantLT {
			t.Errorf("%v.LT(%v) = %v, want %v", c.a, c.b, got, c.wantLT)
		}
	}

	b1, _ := ParseBytes("1Gi")
	b2, _ := ParseBytes("512Mi")
	if !b1.GTE(b2) || b1.LT(b2) {
		t.Errorf("1Gi should be GTE 512Mi and not LT")
	}
	if !b2.LT(b1) {
		t.Errorf("512Mi should be LT 1Gi")
	}
	if !(Bytes{N: 1000}).GTE(Bytes{N: 1000}) {
		t.Errorf("equal Bytes should be GTE")
	}
}
