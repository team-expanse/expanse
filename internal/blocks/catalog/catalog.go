// Package catalog loads shipped block definitions from nix/blocks/<category>/<name>/
// (PHASE04.md §3.3: block.yaml, schema.json, module.nix, defaults.yaml).
// Task T04 fills this in; the import below pins the santhosh-tekuri/jsonschema
// dependency into vendor/ from day one (draft 2020-12, decision D4.1).
package catalog

import (
	_ "github.com/santhosh-tekuri/jsonschema/v5"
)
