package controller

import (
	"crypto/rand"
	"encoding/hex"

	pb "github.com/expanse/expanse/proto"
)

// newPlacementID names a placement for good, so a VM's local save never loads into a later one.
func newPlacementID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // never fails on Linux
	return hex.EncodeToString(b)
}

// namePlacements gives an id to each placement made before ids existed, and reports whether any lacked one.
func namePlacements(status *pb.BlockStatus) bool {
	named := false
	for _, p := range status.GetPlacements() {
		if p.GetPlacementId() == "" {
			p.PlacementId, named = newPlacementID(), true
		}
	}
	return named
}
