// Package conformance provides the shared conformance test suite that every
// Store implementation must pass. Phase 02 runs it against boltstore;
// Phase 03 runs the *same* suite against raftstore. The suite lives in a
// regular (non _test) file so other packages' tests can import it.
package conformance

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// RunConformance runs the full conformance suite against a Store
// implementation. The factory creates a fresh, empty store for each subtest
// and Close is called automatically.
func RunConformance(t *testing.T, newStore func(t *testing.T) store.Store) {
	ctx := context.Background()

	t.Run("GetNotFound", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		_, err := s.Get(ctx, "/no/such/key")
		if err == nil {
			t.Fatal("expected error for missing key")
		}
		if !errors.Is(err, errors.KindNotFound) {
			t.Errorf("error kind = %q, want %q", errors.KindOf(err), errors.KindNotFound)
		}
	})

	t.Run("PutAndGet", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		rev, err := s.Put(ctx, "/foo", []byte("bar"))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		e, err := s.Get(ctx, "/foo")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if e.Key != "/foo" {
			t.Errorf("entry key = %q, want /foo", e.Key)
		}
		if string(e.Value) != "bar" {
			t.Errorf("value = %q, want %q", e.Value, "bar")
		}
		if e.Revision != rev {
			t.Errorf("revision = %d, want %d", e.Revision, rev)
		}
	})

	t.Run("PutOverwrite", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		s.Put(ctx, "/foo", []byte("v1"))
		s.Put(ctx, "/foo", []byte("v2"))
		e, err := s.Get(ctx, "/foo")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(e.Value) != "v2" {
			t.Errorf("value = %q, want v2", e.Value)
		}
		if e.CreatedAt == 0 || e.UpdatedAt == 0 {
			t.Error("timestamps must be set")
		}
	})

	t.Run("ListPrefixIsLiteral", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		s.Put(ctx, "/a/1", []byte("one"))
		s.Put(ctx, "/a/2", []byte("two"))
		s.Put(ctx, "/ab", []byte("prefix-catch"))
		s.Put(ctx, "/b/1", []byte("three"))
		entries, err := s.List(ctx, "/a/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("len(entries) = %d, want 2", len(entries))
		}
		if entries[0].Key != "/a/1" || entries[1].Key != "/a/2" {
			t.Errorf("keys = [%s %s], want sorted [/a/1 /a/2]", entries[0].Key, entries[1].Key)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		s.Put(ctx, "/foo", []byte("bar"))
		if err := s.Delete(ctx, "/foo", 0); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		_, err := s.Get(ctx, "/foo")
		if !errors.Is(err, errors.KindNotFound) {
			t.Errorf("err kind = %q, want not_found", errors.KindOf(err))
		}
		if err := s.Delete(ctx, "/foo", 0); !errors.Is(err, errors.KindNotFound) {
			t.Errorf("delete of missing key: err kind = %q, want not_found", errors.KindOf(err))
		}
	})

	t.Run("DeleteWithRevision", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		r1, _ := s.Put(ctx, "/foo", []byte("v1"))
		s.Put(ctx, "/foo", []byte("v2"))
		if err := s.Delete(ctx, "/foo", r1); !errors.Is(err, errors.KindConflict) {
			t.Errorf("stale delete: err kind = %q, want conflict", errors.KindOf(err))
		}
		e, _ := s.Get(ctx, "/foo")
		if string(e.Value) != "v2" {
			t.Error("stale delete must not remove the key")
		}
		r2, _ := s.Put(ctx, "/foo", []byte("v3"))
		if err := s.Delete(ctx, "/foo", r2); err != nil {
			t.Fatalf("delete with correct revision: %v", err)
		}
	})

	t.Run("CASSuccess", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		r1, _ := s.Put(ctx, "/foo", []byte("v1"))
		r2, err := s.CompareAndSwap(ctx, "/foo", r1, []byte("v2"))
		if err != nil {
			t.Fatalf("CAS: %v", err)
		}
		if r2 <= r1 {
			t.Error("CAS should return a new revision")
		}
		e, _ := s.Get(ctx, "/foo")
		if string(e.Value) != "v2" {
			t.Errorf("value = %q, want v2", e.Value)
		}
	})

	t.Run("CASFailureStale", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		r1, _ := s.Put(ctx, "/foo", []byte("v1"))
		s.Put(ctx, "/foo", []byte("v2")) // advance revision
		_, err := s.CompareAndSwap(ctx, "/foo", r1, []byte("v3"))
		if !errors.Is(err, errors.KindConflict) {
			t.Fatalf("err kind = %q, want conflict", errors.KindOf(err))
		}
		e, _ := s.Get(ctx, "/foo")
		if string(e.Value) != "v2" {
			t.Errorf("value = %q, want unchanged v2", e.Value)
		}
	})

	t.Run("CASMustNotExist", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		s.Put(ctx, "/foo", []byte("v1"))
		if _, err := s.CompareAndSwap(ctx, "/foo", 0, []byte("v2")); !errors.Is(err, errors.KindConflict) {
			t.Errorf("err kind = %q, want conflict", errors.KindOf(err))
		}
		// expect!=0 on a missing key is also a conflict (stale).
		if _, err := s.CompareAndSwap(ctx, "/missing", 5, []byte("v")); !errors.Is(err, errors.KindConflict) {
			t.Errorf("err kind = %q, want conflict", errors.KindOf(err))
		}
	})

	t.Run("CASCreateWithZero", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		rev, err := s.CompareAndSwap(ctx, "/new", 0, []byte("v"))
		if err != nil {
			t.Fatalf("CAS create: %v", err)
		}
		if rev == 0 {
			t.Error("CAS create must return a nonzero revision")
		}
	})

	t.Run("TxnAtomicity", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		r1, _ := s.Put(ctx, "/a", []byte("1"))
		s.Put(ctx, "/b", []byte("x"))
		ops := []store.Op{
			{Kind: store.OpPut, Key: "/a", Value: []byte("2")},
			{Kind: store.OpCheck, Key: "/b", Expect: r1 + 100}, // stale
		}
		if _, err := s.Txn(ctx, ops); !errors.Is(err, errors.KindConflict) {
			t.Fatalf("err kind = %q, want conflict", errors.KindOf(err))
		}
		e, _ := s.Get(ctx, "/a")
		if string(e.Value) != "1" {
			t.Error("txn must not apply partial changes")
		}
	})

	t.Run("TxnSuccess", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ops := []store.Op{
			{Kind: store.OpPut, Key: "/x", Value: []byte("1")},
			{Kind: store.OpPut, Key: "/y", Value: []byte("2")},
		}
		rev, err := s.Txn(ctx, ops)
		if err != nil {
			t.Fatalf("Txn: %v", err)
		}
		e1, _ := s.Get(ctx, "/x")
		e2, _ := s.Get(ctx, "/y")
		if string(e1.Value) != "1" || string(e2.Value) != "2" {
			t.Fatal("txn did not apply both writes")
		}
		if e1.Revision != rev || e2.Revision != rev {
			t.Errorf("revisions mismatch: %d, %d vs %d", e1.Revision, e2.Revision, rev)
		}
	})

	t.Run("TxnCheckSuccess", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		r1, _ := s.Put(ctx, "/b", []byte("x"))
		ops := []store.Op{
			{Kind: store.OpCheck, Key: "/b", Expect: r1},
			{Kind: store.OpPut, Key: "/c", Value: []byte("ok")},
		}
		if _, err := s.Txn(ctx, ops); err != nil {
			t.Fatalf("Txn: %v", err)
		}
		if _, err := s.Get(ctx, "/c"); err != nil {
			t.Error("checked put was not applied")
		}
	})

	t.Run("TxnCheckAbsent", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		// Expect==0 on a missing key passes...
		if _, err := s.Txn(ctx, []store.Op{
			{Kind: store.OpCheck, Key: "/fresh", Expect: 0},
			{Kind: store.OpPut, Key: "/fresh", Value: []byte("v")},
		}); err != nil {
			t.Fatalf("Txn check-absent: %v", err)
		}
		// ...and conflicts once the key exists (join node-ID uniqueness).
		if _, err := s.Txn(ctx, []store.Op{
			{Kind: store.OpCheck, Key: "/fresh", Expect: 0},
			{Kind: store.OpPut, Key: "/other", Value: []byte("x")},
		}); !errors.Is(err, errors.KindConflict) {
			t.Fatalf("err kind = %q, want conflict", errors.KindOf(err))
		}
		if _, err := s.Get(ctx, "/other"); !errors.Is(err, errors.KindNotFound) {
			t.Error("conflicting txn must not apply writes")
		}
	})

	t.Run("TxnDeleteAndWatch", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		s.Put(ctx, "/d", []byte("1"))
		ch, _ := s.Watch(ctx, "/", 0)
		ops := []store.Op{
			{Kind: store.OpPut, Key: "/e", Value: []byte("2")},
			{Kind: store.OpDelete, Key: "/d"},
		}
		rev, err := s.Txn(ctx, ops)
		if err != nil {
			t.Fatalf("Txn: %v", err)
		}
		if rev == 0 {
			t.Error("txn with delete must return a revision")
		}
		got := map[string]store.EventType{}
		for i := 0; i < 2; i++ {
			select {
			case ev := <-ch:
				got[string(ev.Entry.Key)] = ev.Type
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for txn watch events (got %d/2)", i)
			}
		}
		if got["/e"] != store.EventPut || got["/d"] != store.EventDelete {
			t.Errorf("txn watch events = %v, want put /e and delete /d", got)
		}
	})

	t.Run("WatchDeliversEvents", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ch, err := s.Watch(ctx, "/", 0)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		s.Put(ctx, "/a", []byte("1"))
		s.Put(ctx, "/b", []byte("2"))
		for i := 0; i < 2; i++ {
			select {
			case ev, ok := <-ch:
				if !ok {
					t.Fatal("watch channel closed prematurely")
				}
				if ev.Type != store.EventPut {
					t.Errorf("event type = %d, want %d", ev.Type, store.EventPut)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for watch event (got %d/2)", i)
			}
		}
	})

	t.Run("WatchPrefixFilters", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ch, _ := s.Watch(ctx, "/a/", 0)
		s.Put(ctx, "/a/1", []byte("1"))
		s.Put(ctx, "/b/1", []byte("2"))
		select {
		case ev := <-ch:
			if ev.Entry.Key != "/a/1" {
				t.Errorf("got event for %s, want /a/1", ev.Entry.Key)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for filtered watch event")
		}
		select {
		case ev := <-ch:
			t.Errorf("unexpected event for key %s", ev.Entry.Key)
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("WatchFromRev", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		r1, _ := s.Put(ctx, "/k0", []byte("0"))
		ch, err := s.Watch(ctx, "/", r1)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		s.Put(ctx, "/k1", []byte("1"))
		select {
		case ev := <-ch:
			if ev.Entry.Key != "/k1" {
				t.Errorf("first event key = %s, want /k1 (fromRev must exclude earlier events)", ev.Entry.Key)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for watch event after fromRev")
		}
	})

	t.Run("WatchCtxCancelClosesChannel", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		wctx, cancel := context.WithCancel(ctx)
		ch, _ := s.Watch(wctx, "/", 0)
		cancel()
		select {
		case _, ok := <-ch:
			if ok {
				t.Error("expected channel close on ctx cancel")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("watch channel not closed after ctx cancel")
		}
	})

	t.Run("WatchOverflowClosesChannel", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ch, _ := s.Watch(ctx, "/", 0)
		// Fill the buffer past its limit (1024 events) without draining.
		for i := 0; i < 3000; i++ {
			s.Put(ctx, store.Key(fmt.Sprintf("/k%d", i)), []byte("v"))
		}
		// Drain: buffered events may still be delivered, but the channel
		// must be closed (never a silent drop + still-open channel).
		deadline := time.After(5 * time.Second)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return // closed as required
				}
			case <-deadline:
				t.Fatal("watch channel did not close after buffer overflow")
			}
		}
	})

	t.Run("ConcurrentWrites", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					if _, err := s.Put(ctx, store.Key(fmt.Sprintf("/k%d/%d", n, j)), []byte(fmt.Sprintf("v%d", j))); err != nil {
						t.Errorf("Put: %v", err)
					}
				}
			}(i)
		}
		wg.Wait()
		entries, err := s.List(ctx, "/k0/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(entries) != 50 {
			t.Errorf("len(entries) = %d, want 50", len(entries))
		}
	})

	t.Run("MonotonicRevisions", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		var lastRev store.Revision
		for i := 0; i < 100; i++ {
			r, err := s.Put(ctx, store.Key(fmt.Sprintf("/k%d", i)), []byte("v"))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if r <= lastRev {
				t.Fatalf("revision %d not > previous %d", r, lastRev)
			}
			lastRev = r
		}
		cur, err := s.Revision(ctx)
		if err != nil {
			t.Fatalf("Revision: %v", err)
		}
		if cur < lastRev {
			t.Errorf("Revision() = %d < last write revision %d", cur, lastRev)
		}
	})

	t.Run("WatchDeliversInOrder", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		ch, _ := s.Watch(ctx, "/", 0)
		for i := 0; i < 5; i++ {
			s.Put(ctx, store.Key(fmt.Sprintf("/k%d", i)), []byte("v"))
		}
		var lastRev store.Revision
		for i := 0; i < 5; i++ {
			select {
			case ev, ok := <-ch:
				if !ok {
					t.Fatal("watch channel closed prematurely")
				}
				if ev.Revision <= lastRev {
					t.Errorf("revision %d not > previous %d", ev.Revision, lastRev)
				}
				lastRev = ev.Revision
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for watch event (got %d/5)", i)
			}
		}
	})
}
