package vmready

import "testing"

func TestAnUpdateNoteRoundTripsBeforeTheAddresses(t *testing.T) {
	text := WithAddresses(WithUpdateNote(ReadyPrefix+" multi-user.target reached", true), []string{"192.168.1.50"})
	if !StatusTextReady(text) {
		t.Errorf("%q no longer reads as ready", text)
	}
	state, _ := SplitAddresses(text)
	if state, pending := SplitUpdateNote(state); !pending || state != ReadyPrefix+" multi-user.target reached" {
		t.Errorf("SplitUpdateNote = %q, %v", state, pending)
	}
}

func TestWithoutAPendingUpdateTheTextIsUnchanged(t *testing.T) {
	if got := WithUpdateNote("booting", false); got != "booting" {
		t.Errorf("WithUpdateNote = %q, want booting", got)
	}
	if state, pending := SplitUpdateNote("booting"); pending || state != "booting" {
		t.Errorf("SplitUpdateNote = %q, %v", state, pending)
	}
}
