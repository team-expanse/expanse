package chaosstorage

import "testing"

func TestLedgerAcceptsAckedThroughAttempted(t *testing.T) {
	var l Ledger
	l.Attempt(7, 10)
	l.Ack(7, 10)
	l.Attempt(7, 11) // failed write: may or may not have landed
	for _, got := range []uint64{10, 11} {
		if err := l.Check(7, got); err != nil {
			t.Errorf("token %d should be valid: %v", got, err)
		}
	}
}

func TestLedgerFlagsAckedWriteLoss(t *testing.T) {
	var l Ledger
	l.Attempt(3, 20)
	l.Ack(3, 20)
	if err := l.Check(3, 15); err == nil {
		t.Fatal("older token than an acked write must be reported as loss")
	}
	if err := l.Check(3, 0); err == nil {
		t.Fatal("zero block after an acked write must be reported as loss")
	}
}

func TestLedgerFlagsPhantomValue(t *testing.T) {
	var l Ledger
	l.Attempt(3, 20)
	if err := l.Check(3, 21); err == nil {
		t.Fatal("a token never attempted must be reported")
	}
}

func TestLedgerUntouchedBlockMustBeZero(t *testing.T) {
	var l Ledger
	if err := l.Check(9, 0); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(9, 4); err == nil {
		t.Fatal("token in a never-written block must be reported")
	}
}

func TestLedgerBlocksListsAttempted(t *testing.T) {
	var l Ledger
	l.Attempt(5, 1)
	l.Attempt(2, 2)
	if got := l.Blocks(); len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Fatalf("Blocks() = %v, want [2 5]", got)
	}
}
