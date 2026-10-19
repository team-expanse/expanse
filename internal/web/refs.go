package web

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/web/schemaform"
	pb "github.com/expanse/expanse/proto"
)

// refChoices maps each x-expanse-ref field's path to the addresses of the existing blocks,
// other than self ("ns/name"), it can point at.
func (s *Server) refChoices(ctx context.Context, schema []byte, self string) map[string][]schemaform.Choice {
	refs := refFields(schema)
	if len(refs) == 0 {
		return nil
	}
	resp, err := s.blocks.List(ctx, &pb.ListBlocksRequest{})
	if err != nil {
		return nil
	}
	out := map[string][]schemaform.Choice{}
	for _, b := range resp.GetBlocks() {
		port := vipPort(b)
		if port == 0 {
			continue
		}
		full := b.GetMetadata().GetNamespace() + "/" + b.GetMetadata().GetName()
		p, _, ok := vip.Lookup(store.WithStale(ctx), s.store, full)
		if !ok || full == self {
			continue
		}
		for _, f := range refs {
			if f.Ref.Type == "" || f.Ref.Type == b.GetSpec().GetType() {
				out[f.Path] = append(out[f.Path], schemaform.Choice{Value: refAddress(f.Ref.Format, p.Addr(), port), Label: full})
			}
		}
	}
	for _, c := range out {
		sort.Slice(c, func(i, j int) bool { return c[i].Label < c[j].Label })
	}
	return out
}

// vipPort is the first port b exposes on its own address, or 0.
func vipPort(b *pb.Block) int32 {
	for _, p := range b.GetSpec().GetNetwork().GetPorts() {
		if p.GetExpose() == pb.Expose_EXPOSE_VIP {
			return p.GetPort()
		}
	}
	return 0
}

// refAddress writes addr:port as a Ref's format wants it; a URL leaves out its scheme's default port.
func refAddress(format string, addr netip.Addr, port int32) string {
	if format != "url" {
		return netip.AddrPortFrom(addr, uint16(port)).String()
	}
	switch port {
	case 80:
		return "http://" + addr.String()
	case 443:
		return "https://" + addr.String()
	}
	return fmt.Sprintf("http://%s:%d", addr, port)
}

// offerRefs sets each Ref field's choices, nested fields included.
func offerRefs(fields []schemaform.Field, choices map[string][]schemaform.Choice) {
	for i := range fields {
		if fields[i].Ref != nil {
			fields[i].Choices = choices[fields[i].Path]
		}
		offerRefs(fields[i].Fields, choices)
	}
}

// soleRefs returns, as config values, the required top-level Ref fields only one block can fill.
func soleRefs(schema []byte, choices map[string][]schemaform.Choice) map[string]any {
	out := map[string]any{}
	for _, f := range refFields(schema) {
		if c := choices[f.Path]; f.Required && len(c) == 1 && strings.Count(f.Path, "/") == 1 {
			out[strings.TrimPrefix(f.Path, "/")] = c[0].Value
		}
	}
	return out
}

// refFields lists the schema's fields that hold another block's address, nested ones included.
func refFields(schema []byte) []schemaform.Field {
	fields, err := schemaform.Build(schema, cfgPrefix, nil)
	if err != nil {
		return nil
	}
	var refs []schemaform.Field
	for _, f := range flatten(fields) {
		if f.Ref != nil {
			refs = append(refs, f)
		}
	}
	return refs
}
