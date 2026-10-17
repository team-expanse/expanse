package drbd

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
)

// ProcPath is where the loaded DRBD module reports its version.
const ProcPath = "/proc/drbd"

// CheckModule refuses a loaded DRBD older than 9: its drbdadm cannot parse the resources volumes
// write. Enabling storage with a switch loads the kernel's in-tree 8.4; a reboot loads DRBD 9.
func CheckModule(procPath string) error {
	f, err := os.Open(procPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return nil
	}
	fields := strings.Fields(sc.Text()) // "version: 9.2.12 (api:2/proto:86-122)"
	if len(fields) < 2 || fields[0] != "version:" || strings.HasPrefix(fields[1], "9.") {
		return nil
	}
	return experrors.New(experrors.KindUnavailable, "drbd.CheckModule",
		fmt.Sprintf("DRBD %s is loaded but volumes need DRBD 9; reboot the node after enabling storage", fields[1]))
}
