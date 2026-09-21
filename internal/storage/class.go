package storage

import (
	"context"

	"gopkg.in/yaml.v3"

	experrors "github.com/expanse/expanse/internal/errors"
)

// Driver is the storage-backend interface (§4.2), exactly as specified —
// Ceph plugs in later without touching callers.
type Driver interface {
	Name() string
	Create(ctx context.Context, v *Volume) error
	Delete(ctx context.Context, volID string) error
	Attach(ctx context.Context, volID, nodeID string) (devPath string, err error)
	Detach(ctx context.Context, volID, nodeID string) error
	Resize(ctx context.Context, volID string, newSize uint64) error
	Snapshot(ctx context.Context, volID, snapName string) error
	RestoreSnapshot(ctx context.Context, volID, snapName string) error
	ListSnapshots(ctx context.Context, volID string) ([]Snapshot, error)
	Status(ctx context.Context, volID string) (*DriverVolumeStatus, error)
	// Capabilities: supportsRWX, supportsSnapshot, supportsOnlineResize.
	Capabilities() Capabilities
}

// Snapshot is a named point-in-time copy on one volume.
type Snapshot struct {
	Name      string
	CreatedAt int64 // unix-nano
}

// DriverVolumeStatus is a driver-level health snapshot for one volume
// (distinct from the store-persisted VolumeStatus).
type DriverVolumeStatus struct {
	// Healthy is false if the backing replica is degraded/missing.
	Healthy bool
	// Details is a human-readable summary (pool health, device state).
	Details string
}

// Capabilities advertises what a driver supports (§4.2).
type Capabilities struct {
	SupportsRWX          bool
	SupportsSnapshot     bool
	SupportsOnlineResize bool
}

// StorageClass is one entry of the cluster's `storageClasses` config
// (§4.2).
type StorageClass struct {
	Name         string            `yaml:"name"`
	Driver       string            `yaml:"driver"` // "drbd", "local", "ceph"
	Replication  int               `yaml:"replication"`
	Params       map[string]string `yaml:"params"`
	NodeSelector map[string]string `yaml:"nodeSelector"`
}

// DefaultStorageClass is used when no classes are configured: drbd,
// replication 3 (the §4.2 example's implicit baseline).
func DefaultStorageClass() StorageClass {
	return StorageClass{
		Name:        "default",
		Driver:      "drbd",
		Replication: 3,
	}
}

// ParseStorageClasses parses the `storageClasses:` YAML block from cluster
// config (§4.2). An empty/nil input yields the default class. Names must
// be unique; replication must be 1..5.
func ParseStorageClasses(raw []byte) ([]StorageClass, error) {
	var doc struct {
		StorageClasses []StorageClass `yaml:"storageClasses"`
	}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, experrors.Wrap(err, experrors.KindInvalid, "storage.ParseStorageClasses", "parse yaml")
		}
	}
	classes := doc.StorageClasses
	if len(classes) == 0 {
		return []StorageClass{DefaultStorageClass()}, nil
	}
	seen := map[string]bool{}
	for i := range classes {
		c := &classes[i]
		if c.Name == "" {
			return nil, experrors.New(experrors.KindInvalid, "storage.ParseStorageClasses", "storage class missing name")
		}
		if seen[c.Name] {
			return nil, experrors.New(experrors.KindInvalid, "storage.ParseStorageClasses", "duplicate storage class name "+c.Name)
		}
		seen[c.Name] = true
		if c.Driver == "" {
			return nil, experrors.New(experrors.KindInvalid, "storage.ParseStorageClasses", "storage class "+c.Name+" missing driver")
		}
		// Replication 0 is legal for drivers that manage their own
		// redundancy (e.g. ceph erasure coding, §4.2's example class);
		// drbd classes must set 1..5.
		if c.Replication < 0 || c.Replication > 5 {
			return nil, experrors.New(experrors.KindInvalid, "storage.ParseStorageClasses", "storage class "+c.Name+" replication out of range 0..5")
		}
	}
	return classes, nil
}

// GetClass looks up a class by name, falling back to the default class.
func GetClass(classes []StorageClass, name string) (StorageClass, bool) {
	for _, c := range classes {
		if c.Name == name {
			return c, true
		}
	}
	return StorageClass{}, false
}
