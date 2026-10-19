// Package schemaform turns a block type's config JSON Schema into HTML form fields and the
// posted form back into config. Shapes a plain input cannot hold fall back to a YAML editor.
package schemaform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Kind is how one field is edited.
type Kind int

const (
	Text Kind = iota
	Secret
	Integer
	Number
	Bool
	Enum
	List
	Group
	YAML
)

var kindNames = [...]string{"text", "secret", "integer", "number", "bool", "enum", "list", "group", "yaml"}

func (k Kind) String() string { return kindNames[k] }

// Field is one form input, or a Group of them for a nested object.
type Field struct {
	Name     string // form input name: prefix + Path
	Path     string // JSON pointer into config, e.g. "/tls/cert"
	Label    string
	Help     string
	Kind     Kind
	Required bool
	// Options are Enum choices, or a Group's preset labels then CustomPreset; a leading "" means unset.
	Options  []string
	Min, Max string
	Pattern  string
	// Multiline marks text with a contentMediaType (a file's contents), edited in a textarea.
	Multiline bool
	// SSHKeys marks a list of SSH public keys, where gh:<user> stands for a GitHub user's keys.
	SSHKeys bool
	// Ref marks a field that holds another block's address; Choices are the blocks it can take.
	Ref         *Ref
	Choices     []Choice
	Placeholder string // the schema's first example, as the input would hold it
	Value       string
	Checked     bool
	Fields      []Field
	Error       string
}

type prop struct {
	key  string
	node *node
}

// node is the subset of JSON Schema the form understands.
type node struct {
	Type        json.RawMessage `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Enum        []any           `json:"enum"`
	Minimum     *float64        `json:"minimum"`
	Maximum     *float64        `json:"maximum"`
	Pattern     string          `json:"pattern"`
	MediaType   string          `json:"contentMediaType"`
	Examples    []any           `json:"examples"`
	WriteOnly   bool            `json:"writeOnly"`
	Required    []string        `json:"required"`
	Properties  json.RawMessage `json:"properties"`
	Items       *node           `json:"items"`
	Additional  json.RawMessage `json:"additionalProperties"`
	AnyOf       json.RawMessage `json:"anyOf"`
	OneOf       json.RawMessage `json:"oneOf"`
	AllOf       json.RawMessage `json:"allOf"`
	If          json.RawMessage `json:"if"`
	Presets     []preset        `json:"x-expanse-presets"`
	SSHKeys     bool            `json:"x-expanse-ssh-keys"`
	Ref         *Ref            `json:"x-expanse-ref"`

	props []prop
}

// Ref is x-expanse-ref: the field holds the address of a block of Type ("" is any), written as
// Format ("url" for http://addr[:port], "hostport" for addr:port); a YAML field lists them.
type Ref struct {
	Type   string `json:"type"`
	Format string `json:"format"`
}

// Choice is one existing block's address offered for a Ref field.
type Choice struct {
	Value string
	Label string
}

// preset is a ready-made value for an object, offered in a select above its fields.
type preset struct {
	Label string         `json:"label"`
	Value map[string]any `json:"value"`
}

const (
	// PresetSuffix names a group's preset select: the group's input name plus this.
	PresetSuffix = "@preset"
	// CustomPreset is the choice that takes the group's own fields.
	CustomPreset = "custom"
)

func parse(raw []byte) (*node, error) {
	var n node
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, err
	}
	if err := n.loadProps(); err != nil {
		return nil, err
	}
	return &n, nil
}

// loadProps parses properties in schema order (a Go map would lose it), recursing into objects.
func (n *node) loadProps() error {
	if len(n.Properties) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(n.Properties))
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		child, err := parse(raw)
		if err != nil {
			return fmt.Errorf("property %v: %w", tok, err)
		}
		n.props = append(n.props, prop{key: fmt.Sprint(tok), node: child})
	}
	return nil
}

func (n *node) typeName() string {
	var s string
	if json.Unmarshal(n.Type, &s) == nil {
		return s
	}
	return "" // absent, or a list of types
}

func (n *node) kind() Kind {
	if (n.AnyOf != nil && !requiredOnly(n.AnyOf)) || n.OneOf != nil || n.AllOf != nil || n.If != nil {
		return YAML
	}
	if n.Enum != nil {
		for _, e := range n.Enum {
			if _, ok := e.(string); !ok {
				return YAML
			}
		}
		return Enum
	}
	switch n.typeName() {
	case "string":
		if n.WriteOnly {
			return Secret
		}
		return Text
	case "integer":
		return Integer
	case "number":
		return Number
	case "boolean":
		return Bool
	case "array":
		if n.Items != nil && n.Items.typeName() == "string" && n.Items.Enum == nil {
			return List
		}
	case "object":
		if n.props != nil && (n.Additional == nil || string(n.Additional) == "false") {
			return Group
		}
	}
	return YAML
}

// requiredOnly reports whether every anyOf branch only lists required properties ("set one
// of these"): validation enforces it and the fields stay plain inputs.
func requiredOnly(raw json.RawMessage) bool {
	var branches []map[string]json.RawMessage
	if json.Unmarshal(raw, &branches) != nil {
		return false
	}
	for _, b := range branches {
		if _, ok := b["required"]; !ok || len(b) != 1 {
			return false
		}
	}
	return true
}

// ordered lists required properties first, then the rest, each in schema order.
func (n *node) ordered() []prop {
	req := map[string]bool{}
	for _, r := range n.Required {
		req[r] = true
	}
	var first, rest []prop
	for _, p := range n.props {
		if req[p.key] {
			first = append(first, p)
		} else {
			rest = append(rest, p)
		}
	}
	return append(first, rest...)
}

// Build returns the fields for schema, showing values (typically the type's defaults).
func Build(schema []byte, prefix string, values map[string]any) ([]Field, error) {
	root, err := parse(schema)
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return build(root, prefix, "", values)
}

func build(n *node, prefix, path string, values map[string]any) ([]Field, error) {
	req := map[string]bool{}
	for _, r := range n.Required {
		req[r] = true
	}
	var out []Field
	for _, p := range n.ordered() {
		f := Field{
			Name: prefix + path + "/" + p.key, Path: path + "/" + p.key,
			Label: p.node.label(p.key), Help: p.node.Description, Kind: p.node.kind(),
			Required: req[p.key], Pattern: p.node.Pattern,
			Multiline: p.node.MediaType != "", SSHKeys: p.node.SSHKeys, Ref: p.node.Ref,
		}
		if len(p.node.Examples) > 0 {
			ex := Field{Kind: f.Kind}
			if err := ex.show(p.node.Examples[0]); err == nil {
				f.Placeholder = ex.Value
			}
		}
		if p.node.Minimum != nil {
			f.Min = formatNumber(*p.node.Minimum)
		}
		if p.node.Maximum != nil {
			f.Max = formatNumber(*p.node.Maximum)
		}
		v, has := values[p.key]
		switch f.Kind {
		case Enum:
			if !f.Required {
				f.Options = append(f.Options, "")
			}
			for _, e := range p.node.Enum {
				f.Options = append(f.Options, e.(string))
			}
		case Group:
			sub, _ := v.(map[string]any)
			kids, err := build(p.node, prefix, f.Path, sub)
			if err != nil {
				return nil, err
			}
			f.Fields = kids
			f.choosePreset(p.node.Presets, sub)
		}
		if has && v != nil {
			if err := f.show(v); err != nil {
				return nil, fmt.Errorf("%s: %w", f.Path, err)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// choosePreset offers presets in Options and selects the one equal to v, or CustomPreset.
func (f *Field) choosePreset(presets []preset, v map[string]any) {
	if len(presets) == 0 {
		return
	}
	if !f.Required {
		f.Options = append(f.Options, "")
	}
	for _, p := range presets {
		f.Options = append(f.Options, p.Label)
		if v != nil && f.Value == "" && sameJSON(p.Value, v) {
			f.Value = p.Label
		}
	}
	f.Options = append(f.Options, CustomPreset)
	if v != nil && f.Value == "" {
		f.Value = CustomPreset
	}
}

func sameJSON(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// show renders a config value into the field; secrets are never shown.
func (f *Field) show(v any) error {
	switch f.Kind {
	case Secret, Group:
	case Bool:
		f.Checked = v == true
	case Integer, Number:
		switch x := v.(type) {
		case float64:
			f.Value = formatNumber(x)
		default:
			f.Value = fmt.Sprint(x)
		}
	case List:
		items, _ := v.([]any)
		parts := make([]string, 0, len(items))
		for _, it := range items {
			parts = append(parts, fmt.Sprint(it))
		}
		f.Value = strings.Join(parts, "\n")
	case YAML:
		raw, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		f.Value = string(raw)
	default:
		f.Value = fmt.Sprint(v)
	}
	return nil
}

// Decode reads the posted form back into config. Empty optional inputs are left out; bad
// input is reported as "path: message", the shape catalog.ValidateConfig uses.
func Decode(schema []byte, prefix string, form url.Values) (map[string]any, []string) {
	root, err := parse(schema)
	if err != nil {
		return nil, []string{"/: parse schema: " + err.Error()}
	}
	var errs []string
	out := decode(root, prefix, "", form, &errs)
	return out, errs
}

func decode(n *node, prefix, path string, form url.Values, errs *[]string) map[string]any {
	out := map[string]any{}
	for _, p := range n.props {
		fp := path + "/" + p.key
		raw := form.Get(prefix + fp)
		fail := func(msg string) { *errs = append(*errs, fp+": "+msg) }
		switch p.node.kind() {
		case Group:
			if v, chosen := pickPreset(p.node.Presets, form, prefix+fp+PresetSuffix); chosen {
				if v != nil {
					out[p.key] = v
				}
			} else if sub := decode(p.node, prefix, fp, form, errs); len(sub) > 0 {
				out[p.key] = sub
			}
		case Bool:
			out[p.key] = raw == "true" || raw == "on"
		case Secret:
			if raw != "" {
				out[p.key] = raw
			}
		case Integer:
			if s := strings.TrimSpace(raw); s != "" {
				i, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					fail("must be a whole number")
					continue
				}
				out[p.key] = i
			}
		case Number:
			if s := strings.TrimSpace(raw); s != "" {
				x, err := strconv.ParseFloat(s, 64)
				if err != nil {
					fail("must be a number")
					continue
				}
				out[p.key] = x
			}
		case List:
			var items []any
			for _, line := range strings.Split(raw, "\n") {
				if s := strings.TrimSpace(line); s != "" {
					items = append(items, s)
				}
			}
			if items != nil {
				out[p.key] = items
			}
		case YAML:
			if strings.TrimSpace(raw) == "" {
				continue
			}
			var v any
			if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
				fail("invalid YAML: " + err.Error())
				continue
			}
			if v != nil {
				out[p.key] = v
			}
		default:
			if s := strings.TrimSpace(raw); s != "" {
				out[p.key] = s
			}
		}
	}
	return out
}

// pickPreset returns the chosen preset's value, nil for "unset"; chosen is false for
// CustomPreset or no select, when the group's own fields apply.
func pickPreset(presets []preset, form url.Values, name string) (map[string]any, bool) {
	sel, posted := form[name]
	if len(presets) == 0 || !posted || sel[0] == CustomPreset {
		return nil, false
	}
	for _, p := range presets {
		if p.Label == sel[0] {
			out := make(map[string]any, len(p.Value))
			for k, v := range p.Value {
				out[k] = v
			}
			return out, true
		}
	}
	return nil, true
}

// AttachErrors sets each "path: message" error on its field and returns the ones that match none.
func AttachErrors(fields []Field, errs []string) []string {
	var rest []string
	for _, e := range errs {
		path, msg, _ := strings.Cut(e, ": ")
		if strings.HasPrefix(msg, "missing properties: ") {
			msg = "Required."
		}
		if f := find(fields, path); f != nil {
			if f.Error != "" {
				f.Error += "; "
			}
			f.Error += msg
			continue
		}
		rest = append(rest, e)
	}
	return rest
}

func find(fields []Field, path string) *Field {
	for i := range fields {
		if fields[i].Path == path {
			return &fields[i]
		}
		if f := find(fields[i].Fields, path); f != nil {
			return f
		}
	}
	return nil
}

func (n *node) label(key string) string {
	if n.Title != "" {
		return n.Title
	}
	return label(key)
}

func formatNumber(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

// label turns a camelCase key into words, keeping acronyms: "publicSSHPort" → "Public SSH port".
func label(key string) string {
	rs := []rune(key)
	var words []string
	start := 0
	for i := 1; i < len(rs); i++ {
		lowerToUpper := unicode.IsLower(rs[i-1]) && unicode.IsUpper(rs[i])
		acronymEnd := unicode.IsUpper(rs[i-1]) && unicode.IsUpper(rs[i]) && i+1 < len(rs) && unicode.IsLower(rs[i+1])
		if lowerToUpper || acronymEnd {
			words = append(words, string(rs[start:i]))
			start = i
		}
	}
	words = append(words, string(rs[start:]))
	for i, w := range words {
		if w != strings.ToUpper(w) || len(w) == 1 {
			w = strings.ToLower(w)
		}
		if i == 0 {
			r := []rune(w)
			r[0] = unicode.ToUpper(r[0])
			w = string(r)
		}
		words[i] = w
	}
	return strings.Join(words, " ")
}

// Refill sets the fields to the raw posted form, so rejected input can be corrected; secrets stay empty.
func Refill(fields []Field, form url.Values) {
	for i := range fields {
		f := &fields[i]
		switch f.Kind {
		case Group:
			if f.Options != nil {
				f.Value = form.Get(f.Name + PresetSuffix)
			}
			Refill(f.Fields, form)
		case Secret:
		case Bool:
			v := form.Get(f.Name)
			f.Checked = v == "true" || v == "on"
		default:
			f.Value = form.Get(f.Name)
		}
	}
}
