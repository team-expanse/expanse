package volume

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	experrors "github.com/expanse/expanse/internal/errors"
)

var safeDir = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

// SplitBrainMarks is a directory of empty files, one per resource the kernel
// dropped for split-brain. The kernel's handler writes them; the node reads them.
type SplitBrainMarks struct{ Dir string }

// NewSplitBrainMarks creates dir. It must be a plain absolute path because the
// handler quotes nothing.
func NewSplitBrainMarks(dir string) (SplitBrainMarks, error) {
	if !safeDir.MatchString(dir) {
		return SplitBrainMarks{}, experrors.New(experrors.KindInvalid, "volume.split-brain", "marker directory must be a plain absolute path: "+dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return SplitBrainMarks{}, experrors.Wrap(err, experrors.KindInternal, "volume.split-brain", "create "+dir)
	}
	return SplitBrainMarks{Dir: dir}, nil
}

// Handler is the command DRBD runs (through sh) when it drops a split-brain
// connection. It only records: a redirect needs no PATH and resolves nothing.
func (m SplitBrainMarks) Handler() string { return ": > " + m.Dir + "/$DRBD_RESOURCE" }

// Marked reports whether the kernel has reported res split-brained.
func (m SplitBrainMarks) Marked(res string) (bool, error) {
	_, err := os.Stat(filepath.Join(m.Dir, res))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, experrors.Wrap(err, experrors.KindInternal, "volume.split-brain", "read mark of "+res)
}

// Clear forgets res's mark; clearing one that is not there is not an error.
func (m SplitBrainMarks) Clear(res string) error {
	if err := os.Remove(filepath.Join(m.Dir, res)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return experrors.Wrap(err, experrors.KindInternal, "volume.split-brain", "clear mark of "+res)
	}
	return nil
}
