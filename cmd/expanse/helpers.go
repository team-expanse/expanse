package main

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// parseDuration accepts Go duration strings ("30s", "5m") plus bare
// seconds ("90").
func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	var secs int
	if _, err := fmt.Sscanf(s, "%d", &secs); err == nil {
		return time.Duration(secs) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %q", s)
}

func marshalYAML(v any) ([]byte, error) {
	return yaml.Marshal(v)
}
