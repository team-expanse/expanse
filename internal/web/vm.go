package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/blocks/vm"
	"github.com/expanse/expanse/internal/sshkeys"
	"github.com/expanse/expanse/internal/web/schemaform"
	pb "github.com/expanse/expanse/proto"
)

// vmGuest is what the block page tells about a vm/instance guest.
type vmGuest struct {
	MAC      string
	Hostname string
	Login    string // the user to SSH in as, "" when unknown
}

// guestInfo describes b's guest, nil for other types. schema is the type's config schema.
func guestInfo(b *pb.Block, schema []byte) *vmGuest {
	if b.GetSpec().GetType() != "vm/instance" {
		return nil
	}
	cfg := b.GetSpec().GetConfig().AsMap()
	name := b.GetMetadata().GetName()
	g := &vmGuest{MAC: vm.MAC(cfg, systemd.Instance(b.GetMetadata().GetNamespace(), name, 0)), Hostname: name}
	ci, _ := vm.CloudInitFromConfig(cfg)
	if ci.Hostname != "" {
		g.Hostname = ci.Hostname
	}
	g.Login = ci.User
	if g.Login == "" {
		g.Login = presetLogin(schema, cfg["image"])
	}
	return g
}

// typeSchema is type t's config schema, nil when the catalog cannot say.
func (s *Server) typeSchema(ctx context.Context, t string) []byte {
	if s.catalog == nil || t != "vm/instance" {
		return nil
	}
	bt, err := s.catalog.GetType(ctx, &pb.GetTypeRequest{Name: t})
	if err != nil {
		return nil
	}
	return bt.GetSchemaJson()
}

// presetLogin is the default user of the image preset equal to image, if any.
func presetLogin(schema []byte, image any) string {
	var s struct {
		Properties struct {
			Image struct {
				Presets []struct {
					Value any    `json:"value"`
					Login string `json:"login"`
				} `json:"x-expanse-presets"`
			} `json:"image"`
		} `json:"properties"`
	}
	if image == nil || json.Unmarshal(schema, &s) != nil {
		return ""
	}
	want, _ := json.Marshal(image)
	for _, p := range s.Properties.Image.Presets {
		if got, _ := json.Marshal(p.Value); string(got) == string(want) {
			return p.Login
		}
	}
	return ""
}

// importFormKeys runs importKeys over the fields of bt's schema.
func (s *Server) importFormKeys(ctx context.Context, bt *pb.BlockType, cfg map[string]any) []string {
	fields, err := schemaform.Build(bt.GetSchemaJson(), cfgPrefix, nil)
	if err != nil {
		return nil
	}
	return s.importKeys(ctx, fields, cfg)
}

// importKeys replaces each gh:<user> in the SSH key lists of cfg with that GitHub user's
// public keys, returning "path: message" errors for the form.
func (s *Server) importKeys(ctx context.Context, fields []schemaform.Field, cfg map[string]any) []string {
	var errs []string
	for _, f := range flatten(fields) {
		if !f.SSHKeys {
			continue
		}
		parent, key := configParent(cfg, f.Path)
		items, _ := parent[key].([]any)
		var out []any
		for _, it := range items {
			user, ok := strings.CutPrefix(fmt.Sprint(it), "gh:")
			if !ok {
				out = append(out, it)
				continue
			}
			keys, err := sshkeys.GitHub(ctx, s.GitHubURL, user)
			if err != nil {
				errs = append(errs, f.Path+": "+err.Error())
				continue
			}
			for _, k := range keys {
				out = append(out, k)
			}
		}
		if items != nil {
			parent[key] = out
		}
	}
	return errs
}

func flatten(fields []schemaform.Field) []schemaform.Field {
	var out []schemaform.Field
	for _, f := range fields {
		out = append(out, f)
		out = append(out, flatten(f.Fields)...)
	}
	return out
}

// configParent returns the map holding the value at JSON pointer path, and its key there.
func configParent(cfg map[string]any, path string) (map[string]any, string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	m := cfg
	for _, p := range parts[:len(parts)-1] {
		next, _ := m[p].(map[string]any)
		if next == nil {
			return map[string]any{}, parts[len(parts)-1]
		}
		m = next
	}
	return m, parts[len(parts)-1]
}
