package lvm

import (
	"fmt"
	"strconv"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
)

const (
	vgFields = "vg_name,vg_size,vg_free"
	lvFields = "lv_name,vg_name,lv_size,lv_attr,pool_lv,origin,data_percent,metadata_percent"
	sep      = "|"
	attrLen  = 10 // lv_attr is always ten characters
)

// VG is a volume group's capacity in bytes.
type VG struct {
	Name string
	Size uint64
	Free uint64
}

// LVType is the kind of a logical volume, from the first lv_attr character.
type LVType int

const (
	Other    LVType = iota // raid, mirror and other types the engine does not use
	Thick                  // linear LV
	Thin                   // thin volume or thin snapshot
	ThinPool               // thin pool
)

// LV is one visible logical volume. Percentages are 0 when LVM reports none.
type LV struct {
	Name, VG    string
	Size        uint64
	Type        LVType
	Pool        string // thin pool of a thin volume or snapshot
	Origin      string // set for thin snapshots
	Active      bool
	DataPercent float64 // pools and thin volumes
	MetaPercent float64 // pools only
}

func malformed(op, format string, args ...any) error {
	return experrors.New(experrors.KindInternal, op, "malformed lvm output: "+fmt.Sprintf(format, args...))
}

// rows splits report output into trimmed, non-empty lines of n fields each.
func rows(op, out string, n int) ([][]string, error) {
	var res [][]string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		fields := strings.Split(line, sep)
		if len(fields) != n {
			return nil, malformed(op, "want %d fields, got %d in %q", n, len(fields), line)
		}
		res = append(res, fields)
	}
	return res, nil
}

func parseVG(out string) (VG, error) {
	const op = "lvm.parseVG"
	rs, err := rows(op, out, 3)
	if err != nil {
		return VG{}, err
	}
	if len(rs) != 1 {
		return VG{}, malformed(op, "want one volume group, got %d", len(rs))
	}
	size, sizeErr := strconv.ParseUint(rs[0][1], 10, 64)
	free, freeErr := strconv.ParseUint(rs[0][2], 10, 64)
	switch {
	case rs[0][0] == "":
		return VG{}, malformed(op, "empty volume group name")
	case sizeErr != nil || freeErr != nil:
		return VG{}, malformed(op, "non-integer size %q or free %q", rs[0][1], rs[0][2])
	case free > size:
		return VG{}, malformed(op, "free %d exceeds size %d", free, size)
	}
	return VG{Name: rs[0][0], Size: size, Free: free}, nil
}

func parseLVs(out string) ([]LV, error) {
	const op = "lvm.parseLVs"
	rs, err := rows(op, out, 8)
	if err != nil {
		return nil, err
	}
	lvs := make([]LV, 0, len(rs))
	for _, f := range rs {
		lv, err := parseLV(f)
		if err != nil {
			return nil, malformed(op, "%v in %q", err, strings.Join(f, sep))
		}
		lvs = append(lvs, lv)
	}
	return lvs, nil
}

func parseLV(f []string) (LV, error) {
	size, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return LV{}, fmt.Errorf("non-integer size %q", f[2])
	}
	if f[0] == "" || len(f[3]) != attrLen {
		return LV{}, fmt.Errorf("empty name or bad attr %q", f[3])
	}
	data, err := parsePercent(f[6])
	if err != nil {
		return LV{}, err
	}
	meta, err := parsePercent(f[7])
	if err != nil {
		return LV{}, err
	}
	return LV{
		Name: f[0], VG: f[1], Size: size, Type: lvType(f[3][0]), Pool: f[4], Origin: f[5],
		Active: f[3][4] == 'a', DataPercent: data, MetaPercent: meta,
	}, nil
}

func lvType(c byte) LVType {
	switch c {
	case '-':
		return Thick
	case 'V':
		return Thin
	case 't':
		return ThinPool
	}
	return Other
}

func parsePercent(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	p, err := strconv.ParseFloat(s, 64)
	if err != nil || p < 0 || p > 100 {
		return 0, fmt.Errorf("bad percentage %q", s)
	}
	return p, nil
}
