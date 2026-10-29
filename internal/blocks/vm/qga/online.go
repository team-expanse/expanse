package qga

import (
	"context"
	"errors"
)

type vcpu struct {
	ID     int  `json:"logical-id"`
	Online bool `json:"online"`
}

type memBlock struct {
	Index  uint64 `json:"phys-index"`
	Online bool   `json:"online"`
}

// Online brings the guest's offline vCPUs and memory blocks online, for guests that leave
// hot-added ones offline.
func Online(ctx context.Context, sock string) error {
	return errors.Join(onlineVCPUs(ctx, sock), onlineMemory(ctx, sock))
}

func onlineVCPUs(ctx context.Context, sock string) error {
	var all, off []vcpu
	if err := request(ctx, sock, "guest-get-vcpus", nil, &all); err != nil {
		return err
	}
	for _, c := range all {
		if !c.Online {
			off = append(off, vcpu{c.ID, true})
		}
	}
	if len(off) == 0 {
		return nil
	}
	var n int
	return request(ctx, sock, "guest-set-vcpus", map[string]any{"vcpus": off}, &n)
}

func onlineMemory(ctx context.Context, sock string) error {
	var all, off []memBlock
	if err := request(ctx, sock, "guest-get-memory-blocks", nil, &all); err != nil {
		return err
	}
	for _, b := range all {
		if !b.Online {
			off = append(off, memBlock{b.Index, true})
		}
	}
	if len(off) == 0 {
		return nil
	}
	var res []struct{}
	return request(ctx, sock, "guest-set-memory-blocks", map[string]any{"mem-blks": off}, &res)
}
