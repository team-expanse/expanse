package vmready

import (
	"slices"
	"testing"
)

func TestWithAddressesRoundTrips(t *testing.T) {
	text := WithAddresses(ReadyPrefix+" multi-user.target reached", []string{"192.168.1.50", "fd00::5"})
	if !StatusTextReady(text) {
		t.Errorf("%q no longer reads as ready", text)
	}
	state, addrs := SplitAddresses(text)
	if state != ReadyPrefix+" multi-user.target reached" || !slices.Equal(addrs, []string{"192.168.1.50", "fd00::5"}) {
		t.Errorf("SplitAddresses(%q) = %q, %q", text, state, addrs)
	}
}

func TestWithoutAddressesTheTextIsUnchanged(t *testing.T) {
	if got := WithAddresses("booting", nil); got != "booting" {
		t.Errorf("WithAddresses = %q, want booting", got)
	}
	if state, addrs := SplitAddresses("not ready: guest (in parentheses)"); state != "not ready: guest (in parentheses)" || addrs != nil {
		t.Errorf("SplitAddresses = %q, %q", state, addrs)
	}
}
