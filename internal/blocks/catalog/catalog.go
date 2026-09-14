// Package catalog loads shipped block definitions from
// nix/blocks/<category>/<name>/ (PHASE04.md §3.3: block.yaml, schema.json,
// module.nix, defaults.yaml) and validates spec.config against each type's
// JSON Schema (draft 2020-12, decision D4.1) via santhosh-tekuri/jsonschema/v5.
//
// The loaded Catalog implements the validate.Catalog interface consumed by
// the T03 admission rules (V3 type existence, V19 config schema validation);
// the compile-time check lives in the package tests so that production
// catalog.go does not depend on validate.
//
// Load-time strictness: block.yaml and schema.json are required (a block
// without a schema cannot validate config at API-write time); defaults.yaml
// and module.nix are optional at load time — defaults.yaml is data, and
// module.nix is exercised by the nix build, not by the API path.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/santhosh-tekuri/jsonschema/v5"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"
)

// Type is one loaded block type.
type Type struct {
	Category     string
	Name         string
	Version      string
	Description  string
	Icon         string
	Capabilities []string
	Dir          string
	Schema       *jsonschema.Schema
	Defaults     map[string]any
}

// ID is the fully-qualified type id "<category>/<name>".
func (t *Type) ID() string { return t.Category + "/" + t.Name }

// blockYAML mirrors nix/blocks/<cat>/<name>/block.yaml.
type blockYAML struct {
	Name         string   `yaml:"name"`
	Version      string   `yaml:"version"`
	Description  string   `yaml:"description"`
	Icon         string   `yaml:"icon"`
	Capabilities []string `yaml:"capabilities"`
}

// Catalog is the set of block types loaded from a blocks directory.
// It satisfies validate.Catalog.
type Catalog struct {
	types map[string]*Type
}

// Load scans dir for block type definitions laid out as
// <dir>/<category>/<name>/{block.yaml,schema.json,module.nix,defaults.yaml}.
func Load(dir string) (*Catalog, error) {
	categories, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New(errors.KindInvalid, "catalog.Load",
			fmt.Sprintf("read blocks dir %q: %v", dir, err))
	}
	c := &Catalog{types: map[string]*Type{}}
	for _, cat := range categories {
		if !cat.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(dir, cat.Name()))
		if err != nil {
			return nil, errors.New(errors.KindInvalid, "catalog.Load", err.Error())
		}
		for _, name := range names {
			if !name.IsDir() {
				continue
			}
			typ, err := loadType(filepath.Join(dir, cat.Name(), name.Name()), cat.Name(), name.Name())
			if err != nil {
				return nil, err
			}
			c.types[typ.ID()] = typ
		}
	}
	return c, nil
}

func loadType(dir, category, name string) (*Type, error) {
	op := "catalog.Load"
	raw, err := os.ReadFile(filepath.Join(dir, "block.yaml"))
	if err != nil {
		return nil, errors.New(errors.KindInvalid, op,
			fmt.Sprintf("block %s/%s: read block.yaml: %v", category, name, err))
	}
	var meta blockYAML
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return nil, errors.New(errors.KindInvalid, op,
			fmt.Sprintf("block %s/%s: parse block.yaml: %v", category, name, err))
	}
	if meta.Name != name {
		return nil, errors.New(errors.KindInvalid, op,
			fmt.Sprintf("block %s/%s: block.yaml name %q does not match directory", category, name, meta.Name))
	}

	schemaRaw, err := os.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		return nil, errors.New(errors.KindInvalid, op,
			fmt.Sprintf("block %s/%s: read schema.json: %v", category, name, err))
	}
	schema, err := jsonschema.CompileString(
		filepath.Join(dir, "schema.json"), string(schemaRaw))
	if err != nil {
		return nil, errors.New(errors.KindInvalid, op,
			fmt.Sprintf("block %s/%s: compile schema.json: %v", category, name, err))
	}

	typ := &Type{
		Category:     category,
		Name:         name,
		Version:      meta.Version,
		Description:  meta.Description,
		Icon:         meta.Icon,
		Capabilities: meta.Capabilities,
		Dir:          dir,
		Schema:       schema,
	}

	if defRaw, err := os.ReadFile(filepath.Join(dir, "defaults.yaml")); err == nil {
		var defaults map[string]any
		if err := yaml.Unmarshal(defRaw, &defaults); err != nil {
			return nil, errors.New(errors.KindInvalid, op,
				fmt.Sprintf("block %s/%s: parse defaults.yaml: %v", category, name, err))
		}
		typ.Defaults = defaults
	} else if !os.IsNotExist(err) {
		return nil, errors.New(errors.KindInvalid, op, err.Error())
	}
	return typ, nil
}

// HasType reports whether the catalog knows type t ("<category>/<name>").
func (c *Catalog) HasType(t string) bool { _, ok := c.types[t]; return ok }

// Types lists the available type ids, sorted.
func (c *Catalog) Types() []string {
	out := make([]string, 0, len(c.types))
	for id := range c.types {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// GetType returns the loaded type definition.
func (c *Catalog) GetType(t string) (*Type, bool) {
	typ, ok := c.types[t]
	return typ, ok
}

// Defaults returns a copy of the type's default config values.
func (c *Catalog) Defaults(t string) map[string]any {
	typ, ok := c.types[t]
	if !ok || typ.Defaults == nil {
		return nil
	}
	out := make(map[string]any, len(typ.Defaults))
	for k, v := range typ.Defaults {
		out[k] = v
	}
	return out
}

// ValidateConfig validates config against the type's JSON Schema and returns
// one error string per failing field, prefixed with the field path
// (satisfies V19's "failing field path" message requirement).
func (c *Catalog) ValidateConfig(t string, config *structpb.Struct) []string {
	typ, ok := c.types[t]
	if !ok {
		return nil // V19 only runs for known types; V3 flags unknown ones
	}
	var doc any = map[string]any{}
	if config != nil {
		raw, err := config.MarshalJSON()
		if err != nil {
			return []string{fmt.Sprintf("/: %v", err)}
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return []string{fmt.Sprintf("/: %v", err)}
		}
	}
	err := typ.Schema.Validate(doc)
	if err == nil {
		return nil
	}
	e, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []string{fmt.Sprintf("/: %v", err)}
	}
	return fieldErrors("/", e)
}

// fieldErrors flattens a jsonschema error tree into one "path: message"
// string per leaf. `required` leaves live at the object root but name the
// missing properties in their message, so they are expanded to one entry
// per missing property at "<objPath>/<prop>".
func fieldErrors(parent string, e *jsonschema.ValidationError) []string {
	if len(e.Causes) > 0 {
		var out []string
		for _, c := range e.Causes {
			out = append(out, fieldErrors(e.InstanceLocation, c)...)
		}
		return out
	}
	loc := e.InstanceLocation
	if loc == "" {
		loc = parent
	}
	if e.KeywordLocation == "required" || strings.HasSuffix(e.KeywordLocation, "/required") {
		// Message shape: `missing properties: 'a', 'b'`.
		list := strings.TrimPrefix(e.Message, "missing properties: ")
		var out []string
		for _, prop := range strings.Split(list, ", ") {
			prop = strings.Trim(prop, "'\"")
			out = append(out, fmt.Sprintf("%s/%s: %s", loc, prop, e.Message))
		}
		return out
	}
	return []string{fmt.Sprintf("%s: %s", loc, e.Message)}
}
