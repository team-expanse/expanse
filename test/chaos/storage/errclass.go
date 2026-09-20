package chaosstorage

import "strings"

// clearMarkers are the messages a client legitimately sees when a write
// cannot be made durable: the answer is "failed", never a silent loss.
var clearMarkers = []string{
	"not primary for",
	"lease lost",
	"not quorum-durable",
	"primary fenced",
	"device closed",
}

// ClearError reports whether err is a recognised, explicit refusal.
func ClearError(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	for _, m := range clearMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
