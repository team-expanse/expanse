package upgrade

import (
	_ "embed"
	"strings"
)

//go:embed release-key.pub
var releaseKey string

// ReleaseKey is the public key release assets are signed with ("name:base64"; empty in dev builds).
func ReleaseKey() string { return strings.TrimSpace(releaseKey) }
