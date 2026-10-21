package vmready

import "strings"

// addrMark joins a status text to the guest's addresses, which its guest agent reports.
const addrMark = " (addresses: "

// WithAddresses appends addrs to status, keeping its ready prefix.
func WithAddresses(status string, addrs []string) string {
	if len(addrs) == 0 {
		return status
	}
	return status + addrMark + strings.Join(addrs, " ") + ")"
}

// SplitAddresses undoes WithAddresses.
func SplitAddresses(text string) (string, []string) {
	i := strings.LastIndex(text, addrMark)
	if i < 0 || !strings.HasSuffix(text, ")") {
		return text, nil
	}
	return text[:i], strings.Fields(text[i+len(addrMark) : len(text)-1])
}
