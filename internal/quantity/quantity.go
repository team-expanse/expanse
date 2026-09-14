// Package quantity parses cpu/memory/storage quantities (PHASE04.md §3.2,
// rule V9: "cpu/memory quantities parse (500m, 2, 256Mi, 1Gi) — the error
// message must name the bad value").
//
// Two value types: CPU (millicores) and Bytes (bytes). Parsing is strict —
// a value either parses exactly or is rejected with an error quoting the
// input — because these strings travel through validation at admission time
// and later into systemd unit properties (§5.3) and scheduler overcommit
// math (§4.2).
//
// Accepted forms:
//
//	CPU:   "500m" (millicores), "2", "1.5" (cores, decimal allowed)
//	Bytes: "256Mi", "1Gi", "10Gi" (binary: Ki Mi Gi Ti Pi Ei, 1024^n),
//	       "1k", "1M", "1G" (decimal: k M G T P E, 1000^n), "512" (plain bytes)
//
// Notably, byte quantities must be integers — "1.5" and "1.5Gi" are errors.
// Suffixes are case-sensitive: "1m" is not a byte quantity, "1M" is not
// millicores.
package quantity

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/errors"
)

// CPU is a cpu quantity in millicores ("500m" = 500, "2" = 2000).
type CPU struct{ Milli int64 }

// Bytes is a memory/storage quantity in bytes.
type Bytes struct{ N int64 }

// GTE reports whether c is greater than or equal to o (V8: limits ≥ requests).
func (c CPU) GTE(o CPU) bool { return c.Milli >= o.Milli }

// LT reports whether c is strictly less than o.
func (c CPU) LT(o CPU) bool { return c.Milli < o.Milli }

// GTE reports whether b is greater than or equal to o (V8: limits ≥ requests).
func (b Bytes) GTE(o Bytes) bool { return b.N >= o.N }

// LT reports whether b is strictly less than o.
func (b Bytes) LT(o Bytes) bool { return b.N < o.N }

// String renders the canonical display form: whole cores as an integer
// ("2"), fractional as millicores ("1500m"). The form always re-parses to
// the same value.
func (c CPU) String() string {
	if c.Milli%1000 == 0 {
		return strconv.FormatInt(c.Milli/1000, 10)
	}
	return strconv.FormatInt(c.Milli, 10) + "m"
}

// String renders the canonical display form: the largest exact suffix
// (binary preferred), else plain bytes. The form always re-parses to the
// same value.
func (b Bytes) String() string {
	for _, s := range binarySuffixes {
		if b.N%s.multiplier == 0 && b.N != 0 {
			return strconv.FormatInt(b.N/s.multiplier, 10) + s.name
		}
	}
	for _, s := range decimalSuffixes {
		if b.N%s.multiplier == 0 && b.N != 0 {
			return strconv.FormatInt(b.N/s.multiplier, 10) + s.name
		}
	}
	return strconv.FormatInt(b.N, 10)
}

// ParseCPU parses a cpu quantity into millicores.
func ParseCPU(s string) (CPU, error) {
	const op = "quantity.ParseCPU"
	s = strings.TrimSpace(s)
	if s == "" {
		return CPU{}, errors.New(errors.KindInvalid, op, "empty CPU quantity, want e.g. \"500m\", \"2\", \"1.5\"")
	}

	milli := strings.HasSuffix(s, "m")
	num := s
	if milli {
		num = s[:len(s)-1]
	}
	if !validNumber(num, !milli) { // decimal cores allowed only without the "m" suffix
		return CPU{}, errors.New(errors.KindInvalid, op,
			fmt.Sprintf("invalid CPU quantity %q: want an integer or decimal number of cores, optionally suffixed with \"m\" (e.g. \"500m\", \"2\", \"1.5\")", s))
	}

	if milli {
		// Millicores must be an integer.
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil || n > math.MaxInt64/1000 {
			return CPU{}, errors.New(errors.KindInvalid, op, fmt.Sprintf("invalid CPU quantity %q: value too large", s))
		}
		return CPU{Milli: n}, nil
	}

	m, err := coresToMilli(num)
	if err != nil {
		return CPU{}, errors.New(errors.KindInvalid, op, fmt.Sprintf("invalid CPU quantity %q: %v", s, err))
	}
	return CPU{Milli: m}, nil
}

// coresToMilli converts an integer-or-decimal core count (already validated)
// to millicores, rounding half up past the third decimal digit.
func coresToMilli(num string) (int64, error) {
	intPart, fracPart, _ := strings.Cut(num, ".")
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value too large")
	}
	if whole > math.MaxInt64/1000 {
		return 0, fmt.Errorf("value too large")
	}
	milli := whole * 1000
	if fracPart == "" {
		return milli, nil
	}
	// Scale the fractional part to millicores: first three digits exactly,
	// the fourth digit rounds half up. Carry "0.9999" -> 1000.
	head := fracPart
	if len(head) > 4 {
		head = head[:4]
	}
	for len(head) < 4 {
		head += "0"
	}
	frac, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value too large")
	}
	frac = (frac + 5) / 10 // round half up to millicores
	if frac >= 1000 {
		frac -= 1000
		if milli+1000 < 0 { // overflow guard
			return 0, fmt.Errorf("value too large")
		}
		milli += 1000
	}
	return milli + frac, nil
}

// validNumber reports whether s is a non-negative integer, or (when allowDecimal)
// a non-negative decimal with at most one decimal point, in plain ASCII digits.
// It must start with a digit and not end with a lone dot (".5" and "5." are
// invalid, not 0.5 and 5).
func validNumber(s string, allowDecimal bool) bool {
	if s == "" || s[0] < '0' || s[0] > '9' {
		return false
	}
	dots := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
		case c == '.' && allowDecimal:
			dots++
		default:
			return false
		}
	}
	return dots <= 1 && (dots == 0 || s[len(s)-1] != '.')
}

type suffix struct {
	name       string
	multiplier int64
}

// binarySuffixes and decimalSuffixes are ordered by multiplier descending for
// the canonical String() lookup (largest exact suffix wins).
var (
	binarySuffixes = []suffix{
		{"Ei", 1 << 60},
		{"Pi", 1 << 50},
		{"Ti", 1 << 40},
		{"Gi", 1 << 30},
		{"Mi", 1 << 20},
		{"Ki", 1 << 10},
	}
	decimalSuffixes = []suffix{
		{"E", 1e18}, {"P", 1e15}, {"T", 1e12}, {"G", 1e9}, {"M", 1e6}, {"k", 1e3},
	}
)

// ParseBytes parses a memory/storage quantity into bytes. Byte quantities
// must be whole — "1.5Gi" is an error, not 1610612736.
func ParseBytes(s string) (Bytes, error) {
	const op = "quantity.ParseBytes"
	s = strings.TrimSpace(s)
	if s == "" {
		return Bytes{}, errors.New(errors.KindInvalid, op, "empty byte quantity, want e.g. \"256Mi\", \"1Gi\", \"1G\"")
	}

	num, suf := s, ""
	for _, cand := range binarySuffixes {
		if strings.HasSuffix(s, cand.name) {
			suf, num = cand.name, s[:len(s)-len(cand.name)]
			break
		}
	}
	if suf == "" {
		for _, cand := range decimalSuffixes {
			if strings.HasSuffix(s, cand.name) {
				suf, num = cand.name, s[:len(s)-len(cand.name)]
				break
			}
		}
	}

	if !validNumber(num, false) {
		return Bytes{}, errors.New(errors.KindInvalid, op,
			fmt.Sprintf("invalid byte quantity %q: want an integer count of bytes with an optional suffix from Ki Mi Gi Ti Pi Ei (1024^n) or k M G T P E (1000^n), e.g. \"256Mi\", \"1Gi\", \"1G\"", s))
	}

	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return Bytes{}, errors.New(errors.KindInvalid, op, fmt.Sprintf("invalid byte quantity %q: value too large", s))
	}
	if suf == "" {
		return Bytes{N: n}, nil
	}
	mult := multiplierFor(suf)
	if n != 0 && mult != 0 && n > math.MaxInt64/mult {
		return Bytes{}, errors.New(errors.KindInvalid, op, fmt.Sprintf("invalid byte quantity %q: value too large", s))
	}
	return Bytes{N: n * mult}, nil
}

func multiplierFor(name string) int64 {
	for _, s := range binarySuffixes {
		if s.name == name {
			return s.multiplier
		}
	}
	for _, s := range decimalSuffixes {
		if s.name == name {
			return s.multiplier
		}
	}
	return 0
}
