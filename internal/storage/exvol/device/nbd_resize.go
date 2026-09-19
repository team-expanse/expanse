package device

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"

	experrors "github.com/expanse/expanse/internal/errors"
)

// Generic-netlink ABI of the kernel nbd driver (include/uapi/linux/nbd-netlink.h).
const (
	nbdGenlFamily     = "nbd"
	nbdGenlVersion    = 1
	nbdCmdReconfigure = 3
	nbdAttrIndex      = 1 // u32
	nbdAttrSizeBytes  = 2 // u64
)

// ResizeNBD changes the capacity of an attached /dev/nbdN in place. The
// NBD wire protocol cannot renegotiate size, and dropping a connection
// fails whatever I/O is in flight (G6.14); NBD_CMD_RECONFIGURE with a new
// size updates the live block device without touching its sockets.
func ResizeNBD(nbdDev string, sizeBytes int64) error {
	const op = "exvol.nbd.resize"
	idx, err := nbdDeviceIndex(nbdDev)
	if err != nil {
		return experrors.Wrap(err, experrors.KindInvalid, op, "bad device")
	}
	msg, err := nbdReconfigureSizeMessage(idx, uint64(sizeBytes))
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, op, "encode")
	}
	c, err := genetlink.Dial(nil)
	if err != nil {
		return experrors.Wrap(err, experrors.KindUnavailable, op, "genetlink dial")
	}
	defer c.Close() //nolint:errcheck
	fam, err := c.GetFamily(nbdGenlFamily)
	if err != nil {
		return experrors.Wrap(err, experrors.KindUnavailable, op, "nbd genl family (module loaded?)")
	}
	if _, err := c.Execute(msg, fam.ID, netlink.Request|netlink.Acknowledge); err != nil {
		return experrors.Wrap(err, experrors.KindUnavailable, op, "reconfigure "+nbdDev)
	}
	return nil
}

func nbdReconfigureSizeMessage(index uint32, sizeBytes uint64) (genetlink.Message, error) {
	ae := netlink.NewAttributeEncoder()
	ae.Uint32(nbdAttrIndex, index)
	ae.Uint64(nbdAttrSizeBytes, sizeBytes)
	data, err := ae.Encode()
	if err != nil {
		return genetlink.Message{}, err
	}
	return genetlink.Message{
		Header: genetlink.Header{Command: nbdCmdReconfigure, Version: nbdGenlVersion},
		Data:   data,
	}, nil
}

// nbdDeviceIndex extracts N from "/dev/nbdN".
func nbdDeviceIndex(dev string) (uint32, error) {
	n, ok := strings.CutPrefix(dev, "/dev/nbd")
	if !ok {
		return 0, fmt.Errorf("%q is not an /dev/nbdN device", dev)
	}
	idx, err := strconv.ParseUint(n, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an /dev/nbdN device: %w", dev, err)
	}
	return uint32(idx), nil
}
