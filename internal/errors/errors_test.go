package errors

import (
	"testing"
)

func TestNew(t *testing.T) {
	e := New(KindNotFound, "store.Get", "key not found")
	if e.Kind != KindNotFound {
		t.Errorf("expected %q, got %q", KindNotFound, e.Kind)
	}
	if e.Op != "store.Get" {
		t.Errorf("expected op %q, got %q", "store.Get", e.Op)
	}
	expected := "not_found: store.Get:key not found"
	if e.Error() != expected {
		t.Errorf("expected %q, got %q", expected, e.Error())
	}
}

func TestWrapPreservesChain(t *testing.T) {
	base := New(KindNotFound, "store.Get", "missing")
	wrapped := Wrap(base, KindUnavailable, "request", "failed")
	if wrapped.Kind != KindUnavailable {
		t.Errorf("expected %q, got %q", KindUnavailable, wrapped.Kind)
	}
	if wrapped.Err != base {
		t.Error("wrapped.Err should be base")
	}
	if !Is(wrapped, KindUnavailable) {
		t.Error("Is(wrapped, KindUnavailable) should be true")
	}
}

func TestKindOfDeep(t *testing.T) {
	base := New(KindNotFound, "store.Get", "missing")
	l1 := Wrap(base, KindUnavailable, "request", "failed")
	l2 := Wrap(l1, KindInternal, "handler", "crash")
	if KindOf(l2) != KindNotFound {
		t.Errorf("expected %q from 3-level chain, got %q", KindNotFound, KindOf(l2))
	}
}

func TestKindOfNil(t *testing.T) {
	if KindOf(nil) != "" {
		t.Errorf("KindOf(nil) should be empty, got %q", KindOf(nil))
	}
}

func TestIs(t *testing.T) {
	e := New(KindTimeout, "net.Dial", "timeout")
	if !Is(e, KindTimeout) {
		t.Error("Is(e, KindTimeout) should be true")
	}
	if Is(e, KindNotFound) {
		t.Error("Is(e, KindNotFound) should be false")
	}
}

func TestWrapNil(t *testing.T) {
	e := Wrap(nil, KindInternal, "test", "msg")
	if e.Err != nil {
		t.Error("Wrap(nil, ...) should not set Err")
	}
	if e.Kind != KindInternal {
		t.Errorf("expected %q, got %q", KindInternal, e.Kind)
	}
}