package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPendingUpdateShowsInTheGuestsStatus(t *testing.T) {
	var last string
	g := &guestState{notify: func(s string) { last = s }}
	g.setState("ready: up")
	g.setAddrs([]string{"192.168.1.50"})
	g.setUpdatePending(true)
	if want := "ready: up (QEMU update pending) (addresses: 192.168.1.50)"; last != want {
		t.Errorf("status %q, want %q", last, want)
	}
}

func TestWatchRuntimeFlagsANewerQEMU(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var installed atomic.Pointer[string]
	old, newer := "/nix/store/aaa-qemu/bin/qemu-kvm", "/nix/store/bbb-qemu/bin/qemu-kvm"
	installed.Store(&old)
	resolve := func() (string, error) { return *installed.Load(), nil }
	got := make(chan bool, 4)
	go watchRuntime(ctx, old, resolve, 10*time.Millisecond, func(p bool) { got <- p })
	select {
	case p := <-got:
		t.Fatalf("reported %v while the node has the qemu it runs", p)
	case <-time.After(100 * time.Millisecond):
	}
	installed.Store(&newer)
	select {
	case p := <-got:
		if !p {
			t.Fatal("reported no update for a newer qemu")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never noticed the newer qemu")
	}
}
