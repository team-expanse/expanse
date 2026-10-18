package apply

import (
	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/encoding/protojson"
)

// FromTemplate builds a block of type typ from its template.yaml, with config = its defaults overlaid by config.
func FromTemplate(typ *catalog.Type, namespace, name string, config map[string]any) (*pb.Block, error) {
	spec := make(map[string]any, len(typ.Template)+2)
	for k, v := range typ.Template {
		spec[k] = v
	}
	cfg := make(map[string]any, len(typ.Defaults)+len(config))
	for k, v := range typ.Defaults {
		cfg[k] = v
	}
	for k, v := range config {
		cfg[k] = v
	}
	spec["type"] = typ.ID()
	spec["config"] = cfg
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	jsonb, err := yamlToJSON(map[string]any{"metadata": meta, "spec": spec})
	if err != nil {
		return nil, err
	}
	b := &pb.Block{}
	if err := protojson.Unmarshal(jsonb, b); err != nil {
		return nil, errors.Wrap(err, errors.KindInvalid, "apply.FromTemplate", "map template onto block schema")
	}
	return b, nil
}
