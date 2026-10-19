package vm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// headerSize is the part of the disk that decides whether it holds anything: partition
// tables and boot loaders live there, and a fresh thin volume reads as zeros.
const headerSize = 1 << 20

// Blank reports whether dev's first MiB is all zeros, i.e. no image was written yet.
func Blank(dev string) (bool, error) {
	f, err := os.Open(dev)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, headerSize)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return !bytes.ContainsFunc(buf[:n], func(r rune) bool { return r != 0 }), nil
}

// Install writes the image file at src onto dev as a raw disk, converting a qcow2 with
// qemuImg into a raw file in workDir first.
func Install(ctx context.Context, qemuImg, src, dev, workDir string) error {
	format, size, err := probe(src)
	if err != nil {
		return err
	}
	if dsize, err := deviceSize(dev); err != nil {
		return err
	} else if size > dsize {
		return fmt.Errorf("the image needs %d MiB, the disk has %d MiB: make the disk larger", size>>20, dsize>>20)
	}
	if format == "raw" {
		return writeRaw(src, dev, nil)
	}
	tmp, err := os.CreateTemp(workDir, ".raw-*")
	if err != nil {
		return err
	}
	_ = tmp.Close()
	defer os.Remove(tmp.Name())
	out, err := exec.CommandContext(ctx, qemuImg, "convert", "-f", format, "-O", "raw", src, tmp.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return writeRaw(tmp.Name(), dev, nil)
}

// probe tells qcow2 from raw by the qcow2 magic, returning the guest-visible size. A qcow2
// naming a backing or external data file is refused: qemu-img would read that host file.
func probe(p string) (format string, size int64, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := make([]byte, 104)
	n, _ := io.ReadFull(f, h)
	if n < 32 || string(h[:4]) != "QFI\xfb" {
		st, err := f.Stat()
		if err != nil {
			return "", 0, err
		}
		return "raw", st.Size(), nil
	}
	be := binary.BigEndian
	if be.Uint64(h[8:]) != 0 {
		return "", 0, fmt.Errorf("the qcow2 image names a backing file; use a standalone image")
	}
	if be.Uint32(h[4:]) >= 3 && n >= 80 && be.Uint64(h[72:])&(1<<2) != 0 {
		return "", 0, fmt.Errorf("the qcow2 image keeps its data in an external file; use a standalone image")
	}
	return "qcow2", int64(be.Uint64(h[24:])), nil
}

// deviceSize works for block devices and regular files alike.
func deviceSize(p string) (int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Seek(0, io.SeekEnd)
}

// writeRaw copies src's data extents onto dev, the first MiB last, so a write cut short
// leaves the disk blank and the next start writes it again. onWrite hears each offset written.
func writeRaw(src, dev string, onWrite func(off int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if dsize, err := deviceSize(dev); err != nil {
		return err
	} else if size > dsize {
		return fmt.Errorf("the image needs %d MiB, the disk has %d MiB: make the disk larger", size>>20, dsize>>20)
	}
	out, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	copyRange := func(from, to int64) error {
		buf := make([]byte, 4<<20)
		for off := from; off < to; {
			n := min(int64(len(buf)), to-off)
			if _, err := in.ReadAt(buf[:n], off); err != nil {
				return err
			}
			if _, err := out.WriteAt(buf[:n], off); err != nil {
				return err
			}
			if onWrite != nil {
				onWrite(off)
			}
			off += n
		}
		return nil
	}
	for _, e := range extents(in, size) {
		if from := max(e[0], headerSize); from < e[1] {
			if err := copyRange(from, e[1]); err != nil {
				return err
			}
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := copyRange(0, min(size, headerSize)); err != nil {
		return err
	}
	return out.Sync()
}

// extents lists f's data ranges, skipping holes; a filesystem without SEEK_DATA gives one range.
func extents(f *os.File, size int64) [][2]int64 {
	var out [][2]int64
	fd := int(f.Fd())
	for off := int64(0); off < size; {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return [][2]int64{{0, size}}
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			end = size
		}
		out = append(out, [2]int64{start, end})
		off = end
	}
	return out
}
