package lvm

import (
	"reflect"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

func TestParseVG(t *testing.T) {
	got, err := parseVG("  vg0|21474836480|5368709120\n")
	want := VG{Name: "vg0", Size: 21474836480, Free: 5368709120}
	if err != nil || got != want {
		t.Errorf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestParseVGRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"blank lines":   "\n  \n",
		"too few":       "vg0|100\n",
		"too many":      "vg0|100|50|9\n",
		"non-numeric":   "vg0|big|50\n",
		"negative":      "vg0|-1|50\n",
		"suffix left":   "vg0|20.00g|5.00g\n",
		"two vgs":       "vg0|100|50\nvg1|100|50\n",
		"empty name":    "|100|50\n",
		"free > size":   "vg0|100|200\n",
		"decimal bytes": "vg0|100.5|50\n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseVG(out)
			if experrors.KindOf(err) != experrors.KindInternal {
				t.Errorf("want KindInternal, got %v", err)
			}
		})
	}
}

func TestParseLVs(t *testing.T) {
	out := "  pool|vg0|10737418240|twi-a-tz--|||12.50|3.10\n" +
		"  thin1|vg0|2147483648|Vwi-a-tz--|pool||4.25|\n" +
		"  snap1|vg0|2147483648|Vwi---tz-k|pool|thin1||\n" +
		"  thick1|vg0|1073741824|-wi-a-----||||\n" +
		"\n"
	want := []LV{
		{Name: "pool", VG: "vg0", Size: 10737418240, Type: ThinPool, Active: true, DataPercent: 12.5, MetaPercent: 3.1},
		{Name: "thin1", VG: "vg0", Size: 2147483648, Type: Thin, Pool: "pool", Active: true, DataPercent: 4.25},
		{Name: "snap1", VG: "vg0", Size: 2147483648, Type: Thin, Pool: "pool", Origin: "thin1"},
		{Name: "thick1", VG: "vg0", Size: 1073741824, Type: Thick, Active: true},
	}
	got, err := parseLVs(out)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, %v\nwant %+v", got, err, want)
	}
}

func TestParseLVsEmptyOutputIsNoVolumes(t *testing.T) {
	got, err := parseLVs("")
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestParseLVsUnknownTypeIsOtherNotAnError(t *testing.T) {
	got, err := parseLVs("  m1|vg0|1048576|mwi-a-m---||||\n")
	if err != nil || len(got) != 1 || got[0].Type != Other {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestParseLVsRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"too few fields":   "  a|vg0|100|-wi-a-----\n",
		"non-numeric":      "  a|vg0|big|-wi-a-----||||\n",
		"short attr":       "  a|vg0|100|-wi||||\n",
		"bad percent":      "  a|vg0|100|twi-a-tz--|||lots|1.0\n",
		"percent over":     "  a|vg0|100|twi-a-tz--|||100.5|1.0\n",
		"negative pct":     "  a|vg0|100|twi-a-tz--|||-1.0|1.0\n",
		"empty name":       "  |vg0|100|-wi-a-----||||\n",
		"one good one bad": "  a|vg0|100|-wi-a-----||||\n  b|vg0|x|-wi-a-----||||\n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseLVs(out)
			if experrors.KindOf(err) != experrors.KindInternal {
				t.Errorf("want KindInternal, got %v", err)
			}
		})
	}
}
