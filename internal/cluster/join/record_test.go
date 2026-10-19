package join

import "testing"

// svr1: the first node records no API address and joiners record ":7443", so status showed none.
func TestNodeRecordAPITakesItsHostFromTheRaftAddress(t *testing.T) {
	for _, tc := range []struct{ raft, api, want string }{
		{"10.0.0.1:7444", "", "10.0.0.1:7443"},
		{"10.0.0.1:7444", ":7443", "10.0.0.1:7443"},
		{"10.0.0.1:7444", ":9000", "10.0.0.1:9000"},
		{"10.0.0.1:7444", "10.0.0.9:7443", "10.0.0.9:7443"},
		{"[fd00::1]:7444", "", "[fd00::1]:7443"},
		{"", ":7443", ":7443"},
	} {
		if got := (NodeRecord{RaftAddr: tc.raft, APIAddr: tc.api}).API(); got != tc.want {
			t.Errorf("raft %q api %q: API() = %q, want %q", tc.raft, tc.api, got, tc.want)
		}
	}
}
