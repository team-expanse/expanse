package scheduler

import (
	"fmt"

	"github.com/expanse/expanse/internal/quantity"
)

// GrowthMisfit says why node n, which already holds a replica at old's requests, can't hold next's
// larger ones in place ("Code: detail"), or "" when it can. Only requests that grew are checked.
func GrowthMisfit(n NodeView, old, next ReplicaRequest, cfg OvercommitConfig) string {
	oCPU, oMem, oDisk, _, _ := requestQuantities(old)
	cpu, mem, disk, _, errMsg := requestQuantities(next)
	if errMsg != "" {
		return CodeInvalidRequests + ": " + errMsg
	}
	if grow := cpu.Milli - oCPU.Milli; grow > 0 && availCPU(n, cfg).Milli < grow {
		return fmt.Sprintf("%s: node %s has %s cpu free after reserve+overcommit, the new requests need %s more",
			CodeInsufficientCPU, n.ID, availCPU(n, cfg), quantity.CPU{Milli: grow})
	}
	if grow := mem.N - oMem.N; grow > 0 && availMem(n, cfg).N < grow {
		return fmt.Sprintf("%s: node %s has %s memory free, the new requests need %s more",
			CodeInsufficientMemory, n.ID, availMem(n, cfg), quantity.Bytes{N: grow})
	}
	if grow := disk.N - oDisk.N; grow > 0 && n.FreeDisk.N < grow {
		return fmt.Sprintf("%s: node %s has %s disk free, the new volumes need %s more",
			CodeInsufficientDisk, n.ID, n.FreeDisk, quantity.Bytes{N: grow})
	}
	return ""
}
