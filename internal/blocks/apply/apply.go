// Package apply implements the user-facing block.yaml parser for
// `expanse ctl block apply -f block.yaml [--dry-run]` (§7).
//
// Parse turns YAML (or JSON — YAML is a superset) into the proto Block:
// YAML → generic map → JSON → protojson, so nested spec.config lands in
// the google.protobuf.Struct field losslessly and unknown fields are
// rejected (strict parsing, matching the strictness of V-rules).
// It performs no I/O and no store access — dry-run is "Parse +
// validate.Validate", which by construction cannot write.
package apply

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

// Parse reads one YAML (or JSON) document into a Block. Namespace may be
// omitted (defaults at admission); name must be present.
func Parse(r io.Reader) (*pb.Block, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "apply.Parse", "read document")
	}
	var doc any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil && err != io.EOF {
		return nil, errors.Wrap(err, errors.KindInvalid, "apply.Parse", "parse YAML")
	}
	if doc == nil {
		return nil, errors.New(errors.KindInvalid, "apply.Parse", "empty document")
	}
	// Strip the Kubernetes-style envelope keys; they are not part of the
	// Block schema and strict protojson would reject them.
	if m, ok := doc.(map[string]any); ok {
		delete(m, "apiVersion")
		delete(m, "kind")
	}
	jsonb, err := yamlToJSON(doc)
	if err != nil {
		return nil, err
	}
	b := &pb.Block{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(jsonb, b); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "apply.Parse", "map YAML onto block schema")
	}
	if b.GetMetadata().GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "apply.Parse", "metadata.name is required")
	}
	return b, nil
}

// yamlToJSON re-encodes the decoded YAML document as JSON. yaml.v3
// decodes map keys as any; json.Marshal handles string keys and integers.
func yamlToJSON(doc any) ([]byte, error) {
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "apply.Parse", "convert YAML to JSON")
	}
	return out, nil
}
