package schemaform

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/catalog"
)

const testSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["serverName"],
  "properties": {
    "port": {"type": "integer", "minimum": 1, "maximum": 65535, "description": "Listen port"},
    "serverName": {"type": "string", "minLength": 1, "pattern": "^[a-z.]+$"},
    "ratio": {"type": "number", "title": "Ratio (0-1)"},
    "adminPassword": {"type": "string", "writeOnly": true},
    "policy": {"type": "string", "enum": ["lru", "lfu"]},
    "squash": {"enum": ["root", "none"]},
    "readOnly": {"type": "boolean"},
    "buckets": {"type": "array", "items": {"type": "string"}},
    "tls": {"type": "object", "required": ["cert"], "properties": {
      "cert": {"type": "string"}, "port": {"type": "integer"}}},
    "sites": {"type": "array", "items": {"type": "object"}, "examples": [[{"host": "a.example.org"}]]},
    "caddyfile": {"type": "string", "contentMediaType": "text/plain"},
    "env": {"type": "object", "additionalProperties": {"type": "string"}}
  }
}`

func byPath(fields []Field) map[string]Field {
	out := map[string]Field{}
	var walk func([]Field)
	walk = func(fs []Field) {
		for _, f := range fs {
			out[f.Path] = f
			walk(f.Fields)
		}
	}
	walk(fields)
	return out
}

func mustBuild(t *testing.T, schema string, values map[string]any) []Field {
	t.Helper()
	fields, err := Build([]byte(schema), "cfg", values)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestBuildKindsAndOrder(t *testing.T) {
	fields := mustBuild(t, testSchema, nil)
	var order []string
	for _, f := range fields {
		order = append(order, f.Path)
	}
	want := "/serverName /port /ratio /adminPassword /policy /squash /readOnly /buckets /tls /sites /caddyfile /env"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("order (required first, then schema order):\n got %s\nwant %s", got, want)
	}
	f := byPath(fields)
	kinds := map[string]Kind{
		"/serverName": Text, "/port": Integer, "/ratio": Number, "/adminPassword": Secret,
		"/policy": Enum, "/squash": Enum, "/readOnly": Bool, "/buckets": List,
		"/tls": Group, "/tls/cert": Text, "/tls/port": Integer, "/sites": YAML, "/env": YAML,
	}
	for p, k := range kinds {
		if f[p].Kind != k {
			t.Errorf("%s kind = %v, want %v", p, f[p].Kind, k)
		}
	}
	if !f["/caddyfile"].Multiline || f["/serverName"].Multiline {
		t.Error("contentMediaType should make a multi-line text field, and only then")
	}
	if f["/sites"].Placeholder != "- host: a.example.org\n" {
		t.Errorf("sites placeholder = %q, want its first example as YAML", f["/sites"].Placeholder)
	}
	if !f["/serverName"].Required || f["/port"].Required || !f["/tls/cert"].Required {
		t.Error("required flags wrong")
	}
	if f["/port"].Label != "Port" || f["/port"].Help != "Listen port" || f["/port"].Min != "1" || f["/port"].Max != "65535" {
		t.Errorf("port field = %+v", f["/port"])
	}
	if f["/ratio"].Label != "Ratio (0-1)" {
		t.Errorf("a schema title should be the label, got %q", f["/ratio"].Label)
	}
	if f["/adminPassword"].Label != "Admin password" {
		t.Errorf("label = %q", f["/adminPassword"].Label)
	}
	if f["/serverName"].Pattern != "^[a-z.]+$" || f["/serverName"].Name != "cfg/serverName" {
		t.Errorf("serverName field = %+v", f["/serverName"])
	}
	if got := strings.Join(f["/policy"].Options, ","); got != ",lru,lfu" {
		t.Errorf("optional enum options = %q, want a leading blank (unset)", got)
	}
}

func TestBuildRendersValues(t *testing.T) {
	f := byPath(mustBuild(t, testSchema, map[string]any{
		"port": float64(8080), "ratio": 0.5, "serverName": "a.b", "readOnly": true,
		"buckets": []any{"x", "y"}, "tls": map[string]any{"cert": "/c"},
		"sites": []any{map[string]any{"host": "h"}}, "adminPassword": "hunter22",
	}))
	cases := map[string]string{
		"/port": "8080", "/ratio": "0.5", "/serverName": "a.b", "/buckets": "x\ny", "/tls/cert": "/c",
		"/sites": "- host: h\n", "/adminPassword": "",
	}
	for p, want := range cases {
		if f[p].Value != want {
			t.Errorf("%s value = %q, want %q", p, f[p].Value, want)
		}
	}
	if !f["/readOnly"].Checked {
		t.Error("readOnly not checked")
	}
}

func TestDecode(t *testing.T) {
	form := url.Values{
		"cfg/serverName":    {"a.b"},
		"cfg/port":          {" 8080 "},
		"cfg/ratio":         {""},
		"cfg/adminPassword": {"s3cret"},
		"cfg/policy":        {""},
		"cfg/readOnly":      {"true"},
		"cfg/buckets":       {"x\r\n\n y \n"},
		"cfg/tls/cert":      {""},
		"cfg/tls/port":      {""},
		"cfg/sites":         {"- host: h\n"},
		"cfg/env":           {""},
	}
	got, errs := Decode([]byte(testSchema), "cfg", form)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	want := map[string]any{
		"serverName": "a.b", "port": int64(8080), "adminPassword": "s3cret", "readOnly": true,
		"buckets": []any{"x", "y"}, "sites": []any{map[string]any{"host": "h"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Decode =\n%#v\nwant\n%#v", got, want)
	}
}

func TestDecodeUncheckedBoolIsFalse(t *testing.T) {
	got, _ := Decode([]byte(testSchema), "cfg", url.Values{"cfg/serverName": {"a"}})
	if got["readOnly"] != false {
		t.Errorf("readOnly = %v, want false", got["readOnly"])
	}
}

func TestDecodeReportsBadInputAtItsPath(t *testing.T) {
	_, errs := Decode([]byte(testSchema), "cfg", url.Values{
		"cfg/port": {"eighty"}, "cfg/tls/port": {"1.5"}, "cfg/sites": {"[unclosed"},
	})
	joined := strings.Join(errs, "\n")
	for _, p := range []string{"/port: ", "/tls/port: ", "/sites: "} {
		if !strings.Contains(joined, p) {
			t.Errorf("no error at %s in:\n%s", p, joined)
		}
	}
}

func TestAttachErrors(t *testing.T) {
	fields := mustBuild(t, testSchema, nil)
	rest := AttachErrors(fields, []string{
		"/port: must be <= 65535", "/tls/cert: missing properties: 'cert'", "/: anyOf failed",
	})
	f := byPath(fields)
	if f["/tls/cert"].Error != "Required." {
		t.Errorf("a missing required field reads %q, want Required.", f["/tls/cert"].Error)
	}
	if f["/port"].Error != "must be <= 65535" || f["/tls/cert"].Error == "" {
		t.Errorf("errors not attached: port=%q cert=%q", f["/port"].Error, f["/tls/cert"].Error)
	}
	if len(rest) != 1 || rest[0] != "/: anyOf failed" {
		t.Errorf("unattached = %v", rest)
	}
}

// Every shipped type's defaults survive render → browser post → decode unchanged.
func TestEveryShippedTypeRoundTrips(t *testing.T) {
	cat, err := catalog.Load("../../../nix/blocks")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cat.Types() {
		t.Run(id, func(t *testing.T) {
			typ, _ := cat.GetType(id)
			fields, err := Build(typ.SchemaJSON, "cfg", typ.Defaults)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range byPath(fields) {
				if f.Kind == Bool && typ.Defaults[strings.TrimPrefix(f.Path, "/")] == nil && !strings.Contains(f.Path[1:], "/") {
					t.Errorf("boolean %s has no value in defaults.yaml, so the form cannot show its real default", f.Path)
				}
			}
			got, errs := Decode(typ.SchemaJSON, "cfg", post(fields))
			if len(errs) != 0 {
				t.Fatalf("errs = %v", errs)
			}
			if a, b := normalize(t, got), normalize(t, typ.Defaults); !reflect.DeepEqual(a, b) {
				t.Errorf("round trip changed the config:\n got %v\nwant %v", a, b)
			}
		})
	}
}

// post is what a browser submits for the rendered fields: unchecked boxes send nothing.
func post(fields []Field) url.Values {
	v := url.Values{}
	for _, f := range byPath(fields) {
		switch f.Kind {
		case Group:
			if f.Options != nil {
				v.Set(f.Name+PresetSuffix, f.Value)
			}
		case Bool:
			if f.Checked {
				v.Set(f.Name, "true")
			}
		default:
			v.Set(f.Name, f.Value)
		}
	}
	return v
}

func normalize(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range out { // an empty list and an absent one mean the same
		if l, ok := v.([]any); ok && len(l) == 0 {
			delete(out, k)
		}
	}
	return out
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"port": "Port", "rootURL": "Root URL", "publicSSHPort": "Public SSH port",
		"maxWalSenders": "Max wal senders", "accessKeyId": "Access key id", "iqn": "Iqn",
	} {
		if got := label(in); got != want {
			t.Errorf("label(%q) = %q, want %q", in, got, want)
		}
	}
}

// Refill shows what the user typed, even input Decode rejected, but never a secret.
func TestRefill(t *testing.T) {
	fields := mustBuild(t, testSchema, map[string]any{"port": float64(80), "readOnly": true})
	Refill(fields, url.Values{"cfg/port": {"eighty"}, "cfg/tls/cert": {"/c"}, "cfg/adminPassword": {"pw"}})
	f := byPath(fields)
	if f["/port"].Value != "eighty" || f["/tls/cert"].Value != "/c" || f["/adminPassword"].Value != "" {
		t.Errorf("port=%q cert=%q password=%q", f["/port"].Value, f["/tls/cert"].Value, f["/adminPassword"].Value)
	}
	if f["/readOnly"].Checked {
		t.Error("an unposted checkbox stayed checked")
	}
}

const presetSchema = `{
  "type": "object",
  "properties": {
    "image": {
      "type": "object", "required": ["url"],
      "anyOf": [{"required": ["checksum"]}, {"required": ["checksumUrl"]}],
      "x-expanse-presets": [
        {"label": "Debian", "value": {"url": "https://d/deb.qcow2", "checksumUrl": "https://d/SUMS"}},
        {"label": "Ubuntu", "value": {"url": "https://u/ubu.img", "checksumUrl": "https://u/SUMS"}}
      ],
      "properties": {"url": {"type": "string"}, "checksum": {"type": "string"}, "checksumUrl": {"type": "string"}}
    }
  }
}`

func TestARequiredOneOfStaysAGroup(t *testing.T) {
	if f := byPath(mustBuild(t, presetSchema, nil))["/image"]; f.Kind != Group {
		t.Errorf("kind = %v: an anyOf of required lists is still a plain object", f.Kind)
	}
}

func TestPresetsBuild(t *testing.T) {
	f := byPath(mustBuild(t, presetSchema, map[string]any{"image": map[string]any{"url": "https://u/ubu.img", "checksumUrl": "https://u/SUMS"}}))["/image"]
	if want := []string{"", "Debian", "Ubuntu", CustomPreset}; !reflect.DeepEqual(f.Options, want) {
		t.Errorf("options = %q, want %q", f.Options, want)
	}
	if f.Value != "Ubuntu" {
		t.Errorf("selected %q, want the preset the value matches", f.Value)
	}
	f = byPath(mustBuild(t, presetSchema, map[string]any{"image": map[string]any{"url": "https://mine/x.img", "checksum": "ab"}}))["/image"]
	if f.Value != CustomPreset || byPath(f.Fields)["/image/url"].Value != "https://mine/x.img" {
		t.Errorf("own image: selected %q", f.Value)
	}
	if f := byPath(mustBuild(t, presetSchema, nil))["/image"]; f.Value != "" {
		t.Errorf("unset: selected %q", f.Value)
	}
}

func TestPresetsDecode(t *testing.T) {
	form := url.Values{"cfg/image" + PresetSuffix: {"Debian"}, "cfg/image/url": {"https://ignored"}}
	got, errs := Decode([]byte(presetSchema), "cfg", form)
	want := map[string]any{"url": "https://d/deb.qcow2", "checksumUrl": "https://d/SUMS"}
	if len(errs) != 0 || !reflect.DeepEqual(got["image"], want) {
		t.Errorf("preset: %v, %v", got, errs)
	}
	form = url.Values{"cfg/image" + PresetSuffix: {CustomPreset}, "cfg/image/url": {"https://mine/x.img"}, "cfg/image/checksum": {"ab"}}
	got, _ = Decode([]byte(presetSchema), "cfg", form)
	if want := map[string]any{"url": "https://mine/x.img", "checksum": "ab"}; !reflect.DeepEqual(got["image"], want) {
		t.Errorf("custom: %v", got)
	}
	form = url.Values{"cfg/image" + PresetSuffix: {""}, "cfg/image/url": {"https://left/over"}}
	if got, _ = Decode([]byte(presetSchema), "cfg", form); got["image"] != nil {
		t.Errorf("none: %v", got)
	}
}

func TestPresetsRefill(t *testing.T) {
	fields := mustBuild(t, presetSchema, nil)
	Refill(fields, url.Values{"cfg/image" + PresetSuffix: {CustomPreset}})
	if f := byPath(fields)["/image"]; f.Value != CustomPreset {
		t.Errorf("refilled %q", f.Value)
	}
}
