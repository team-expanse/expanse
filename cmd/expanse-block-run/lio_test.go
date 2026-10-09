package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// recordTargetcli records every targetcli command line instead of running it.
func recordTargetcli(calls *[]string) func(context.Context, ...string) error {
	return func(_ context.Context, args ...string) error {
		*calls = append(*calls, strings.Join(args, " "))
		return nil
	}
}

func TestLIOSetupOpensThePortalLast(t *testing.T) {
	for _, chap := range []string{"", "alice"} {
		var calls []string
		l := &lioTarget{
			backstore: "bs", iqn: "iqn.x:t", wwn: "w", device: "/dev/drbd1", port: "3260",
			chapUser: chap, chapPassword: "secret", run: recordTargetcli(&calls),
		}
		if err := l.setup(context.Background()); err != nil {
			t.Fatal(err)
		}
		// An initiator that connects as soon as 3260 answers must find the target fully configured.
		create := slices.Index(calls, "/iscsi/iqn.x:t/tpg1/portals create 0.0.0.0 3260")
		if create != len(calls)-1 {
			t.Errorf("chap=%q: portal created at step %d of %d:\n%s", chap, create+1, len(calls), strings.Join(calls, "\n"))
		}
	}
}
