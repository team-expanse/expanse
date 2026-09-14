package scheduler

import (
	pb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/quantity"
)

type (
	quantityCPU   = quantity.CPU
	quantityBytes = quantity.Bytes
)

func parseCPU(s string) (quantity.CPU, error)     { return quantity.ParseCPU(s) }
func parseBytes(s string) (quantity.Bytes, error) { return quantity.ParseBytes(s) }

type (
	pbStorage      = pb.Storage
	pbResources    = pb.Resources
	pbResourcePair = pb.ResourcePair
	pbDevice       = pb.Device
)
