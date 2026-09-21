package storage

import (
	"reflect"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

func TestParseStorageClassesSpecExample(t *testing.T) {
	raw := []byte(`
storageClasses:
  - name: default
    driver: drbd
    replication: 3
    params: { compression: zstd, recordsize: 128k, sync: standard }
  - name: fast
    driver: drbd
    replication: 2
    params: { compression: lz4, recordsize: 16k, sync: always }
    nodeSelector: { disk: ssd }
  - name: local
    driver: local
    replication: 1
  - name: ceph
    driver: ceph
    params: { pool: expanse, erasureCode: "4+2" }
`)
	classes, err := ParseStorageClasses(raw)
	if err != nil {
		t.Fatalf("ParseStorageClasses: %v", err)
	}
	if len(classes) != 4 {
		t.Fatalf("got %d classes, want 4", len(classes))
	}
	if classes[0].Name != "default" || classes[0].Driver != "drbd" || classes[0].Replication != 3 {
		t.Errorf("default class: %+v", classes[0])
	}
	if classes[1].Name != "fast" || classes[1].Replication != 2 {
		t.Errorf("fast class: %+v", classes[1])
	}
	if !reflect.DeepEqual(classes[1].Params, map[string]string{"compression": "lz4", "recordsize": "16k", "sync": "always"}) {
		t.Errorf("fast params: %+v", classes[1].Params)
	}
	if !reflect.DeepEqual(classes[1].NodeSelector, map[string]string{"disk": "ssd"}) {
		t.Errorf("fast nodeSelector: %+v", classes[1].NodeSelector)
	}
	if classes[2].Name != "local" || classes[2].Driver != "local" || classes[2].Replication != 1 {
		t.Errorf("local class: %+v", classes[2])
	}
	if classes[3].Name != "ceph" || classes[3].Driver != "ceph" {
		t.Errorf("ceph class: %+v", classes[3])
	}
	if !reflect.DeepEqual(classes[3].Params, map[string]string{"pool": "expanse", "erasureCode": "4+2"}) {
		t.Errorf("ceph params: %+v", classes[3].Params)
	}
}

func TestParseStorageClassesEmptyYieldsDefault(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("storageClasses: []")} {
		classes, err := ParseStorageClasses(raw)
		if err != nil {
			t.Fatalf("ParseStorageClasses(%q): %v", raw, err)
		}
		if len(classes) != 1 || !reflect.DeepEqual(classes[0], DefaultStorageClass()) {
			t.Errorf("expected single default class, got %+v", classes)
		}
	}
}

func TestParseStorageClassesDefaults(t *testing.T) {
	dc := DefaultStorageClass()
	if dc.Name != "default" || dc.Driver != "drbd" || dc.Replication != 3 {
		t.Errorf("unexpected defaults: %+v", dc)
	}
	if len(dc.Params) != 0 {
		t.Errorf("default class has no driver params, got %+v", dc.Params)
	}
}

func TestParseStorageClassesValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"missing name", `
storageClasses:
  - driver: drbd
    replication: 3`},
		{"missing driver", `
storageClasses:
  - name: x
    replication: 3`},
		{"replication too high", `
storageClasses:
  - name: x
    driver: drbd
    replication: 6`},
		{"replication negative", `
storageClasses:
  - name: x
    driver: drbd
    replication: -1`},
		{"duplicate name", `
storageClasses:
  - name: x
    driver: drbd
    replication: 3
  - name: x
    driver: drbd
    replication: 2`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseStorageClasses([]byte(tc.raw))
			if experrors.KindOf(err) != experrors.KindInvalid {
				t.Errorf("KindOf = %v, want invalid (err: %v)", experrors.KindOf(err), err)
			}
		})
	}
}

func TestGetClass(t *testing.T) {
	classes := []StorageClass{
		{Name: "default", Driver: "drbd", Replication: 3},
		{Name: "fast", Driver: "drbd", Replication: 2},
	}
	got, ok := GetClass(classes, "fast")
	if !ok || got.Replication != 2 {
		t.Errorf("GetClass(fast): %+v ok=%v", got, ok)
	}
	_, ok = GetClass(classes, "nope")
	if ok {
		t.Error("GetClass(nope) should miss")
	}
	// Empty list falls back gracefully (no default injected here — callers
	// decide; ParseStorageClasses is where the default originates).
	_, ok = GetClass(nil, "default")
	if ok {
		t.Error("GetClass(nil) should miss")
	}
}
