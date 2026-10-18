package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// credentialPrefix names the credentials the unit loads from the agent's secret
// directory (LoadCredential=config:<dir>): one per config field, config_<field>.
const credentialPrefix = "config_"

// withCredentials puts the block's secrets, handed over by systemd in dir
// ($CREDENTIALS_DIRECTORY), back into its --config JSON.
func withCredentials(args []string, dir string) ([]string, error) {
	if dir == "" {
		return args, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	secrets := map[string]string{}
	for _, e := range ents {
		if field, ok := strings.CutPrefix(e.Name(), credentialPrefix); ok && field != "" {
			v, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("read credential %s: %w", field, err)
			}
			secrets[field] = string(v)
		}
	}
	if len(secrets) == 0 {
		return args, nil
	}
	out := slices.Clone(args)
	i := slices.Index(out, "--config")
	if i < 0 || i+1 >= len(out) {
		out = append(out, "--config", "{}")
		i = len(out) - 2
	}
	cfg := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(out[i+1]))
	dec.UseNumber() // numbers pass through as written
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("bad --config: %w", err)
	}
	for k, v := range secrets {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	out[i+1] = string(raw)
	return out, nil
}
