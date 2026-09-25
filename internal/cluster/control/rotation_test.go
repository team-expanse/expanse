package control_test

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestInitWritesCATrust covers Init's new Phase 10 X2 side effect: the
// Raft-authoritative CATrust record exists right after bootstrap and
// matches the disk-cached CA every prior phase already relied on.
func TestInitWritesCATrust(t *testing.T) {
	r := newRig(t, "ca-init")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	trust, _, err := control.LoadCATrust(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("LoadCATrust: %v", err)
	}
	if trust.Outgoing != nil {
		t.Fatalf("fresh cluster should not be rotating: %+v", trust)
	}
	primary, err := ca.UnmarshalCert(trust.Primary.CertPEM)
	if err != nil {
		t.Fatalf("unmarshal primary: %v", err)
	}
	if primary.SerialNumber.Cmp(r.initRes.CA.Cert.SerialNumber) != 0 {
		t.Errorf("CATrust primary != disk CA")
	}

	status, err := control.CARotationStatus(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("CARotationStatus: %v", err)
	}
	if status.Rotating {
		t.Error("fresh cluster reports Rotating = true")
	}
}

// TestMaybeRenewNoop covers the common-case tick: a fresh cert, signed
// by the current primary, needs nothing.
func TestMaybeRenewNoop(t *testing.T) {
	r := newRig(t, "renew-noop")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	renewed, err := control.MaybeRenew(ctx, r.initRes.Store, r.dataDir, "n1", r.initRes.Secret, time.Now())
	if err != nil {
		t.Fatalf("MaybeRenew: %v", err)
	}
	if renewed {
		t.Error("a fresh cert on the current primary should not renew")
	}
}

// TestMaybeRenewRoutineExpiry covers ordinary time-driven renewal (no
// rotation involved): a `now` within ca.NodeCertRenewLeft of the cert's
// expiry forces reissue.
func TestMaybeRenewRoutineExpiry(t *testing.T) {
	r := newRig(t, "renew-expiry")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	soon := time.Now().Add(ca.NodeCertValidity - ca.NodeCertRenewLeft + time.Hour)
	renewed, err := control.MaybeRenew(ctx, r.initRes.Store, r.dataDir, "n1", r.initRes.Secret, soon)
	if err != nil {
		t.Fatalf("MaybeRenew: %v", err)
	}
	if !renewed {
		t.Fatal("cert within the renewal window should have renewed")
	}

	_, _, clusterCA, err := control.LoadCluster(r.dataDir)
	if err != nil {
		t.Fatalf("LoadCluster after renewal: %v", err)
	}
	if !clusterCA.Cert.NotAfter.After(soon.Add(ca.NodeCertValidity - time.Hour)) {
		t.Error("reissued cert does not look freshly issued")
	}

	// A second call at the same `now` is a no-op (already renewed).
	renewed, err = control.MaybeRenew(ctx, r.initRes.Store, r.dataDir, "n1", r.initRes.Secret, soon)
	if err != nil {
		t.Fatalf("MaybeRenew (2nd): %v", err)
	}
	if renewed {
		t.Error("re-renewed a cert that was just freshly issued")
	}
}

// TestRotateCARefusesConcurrentRotation covers RotateCA's guard: only
// one rotation in flight at a time.
func TestRotateCARefusesConcurrentRotation(t *testing.T) {
	r := newRig(t, "rotate-guard")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := control.RotateCA(ctx, r.initRes.Store, r.initRes.Secret, time.Now()); err != nil {
		t.Fatalf("first RotateCA: %v", err)
	}
	if _, err := control.RotateCA(ctx, r.initRes.Store, r.initRes.Secret, time.Now()); !errors.Is(err, errors.KindConflict) {
		t.Errorf("second RotateCA: err=%v, want conflict", err)
	}
}

// TestCARotationEndToEnd is the X2 mechanism proved at unit-test
// granularity: a 3-node cluster rotates to a fresh CA, every node's
// renewal loop catches up (modeled here as direct MaybeRenew calls,
// since the loop itself is just a ticker around it), mTLS config built
// from BEFORE the rotation keeps working throughout with no rebuild
// (the "no restart" half of X2's zero-downtime claim, proved with a
// real handshake), CompleteCARotation refuses until every node has
// caught up, and once it succeeds the old CA is no longer trusted.
func TestCARotationEndToEnd(t *testing.T) {
	r := newRig(t, "rotate-e2e")
	n2 := r.enrollNode("n2", "voter")
	n3 := r.enrollNode("n3", "voter")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Built once, BEFORE rotation starts — must still work after, with
	// no rebuild, proving the dynamic (no-restart) reload path.
	preRotationSrv, err := control.InternalTLS(ctx, r.initRes.Store, r.initRes.CA, r.dataDir)
	if err != nil {
		t.Fatalf("InternalTLS (pre-rotation): %v", err)
	}
	preRotationCli, err := control.InternalClientTLS(ctx, r.initRes.Store, r.initRes.CA, n2.DataDir)
	if err != nil {
		t.Fatalf("InternalClientTLS (pre-rotation): %v", err)
	}

	oldCACert := r.initRes.CA.Cert

	fresh, err := control.RotateCA(ctx, r.initRes.Store, r.initRes.Secret, time.Now())
	if err != nil {
		t.Fatalf("RotateCA: %v", err)
	}
	if fresh.Cert.SerialNumber.Cmp(oldCACert.SerialNumber) == 0 {
		t.Fatal("RotateCA returned the same CA")
	}

	status, err := control.CARotationStatus(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("CARotationStatus: %v", err)
	}
	if !status.Rotating || len(status.Pending) != 3 {
		t.Fatalf("status after rotate = %+v, want Rotating with 3 pending", status)
	}

	// mTLS built before rotation still works immediately after: n2's
	// old-CA cert is still valid and the old CA is still trusted
	// (Outgoing), and the live membership check succeeds.
	if err := dialWithConfigs(t, r.initRes.Store, preRotationSrv, preRotationCli); err != nil {
		t.Errorf("pre-rotation mTLS config broke immediately after RotateCA: %v", err)
	}

	// Every node catches up (the renewal loop's real job — modeled
	// directly here since the loop is just a ticker around MaybeRenew).
	for _, nr := range []struct{ id, dataDir string }{
		{"n1", r.dataDir}, {"n2", n2.DataDir}, {"n3", n3.DataDir},
	} {
		renewed, err := control.MaybeRenew(ctx, r.initRes.Store, nr.dataDir, nr.id, r.initRes.Secret, time.Now())
		if err != nil {
			t.Fatalf("MaybeRenew(%s): %v", nr.id, err)
		}
		if !renewed {
			t.Errorf("MaybeRenew(%s) should have renewed onto the new primary", nr.id)
		}
	}

	// Same pre-built configs, dialed again after every node renewed:
	// still works — the outgoing CA is still in the trust bundle, and
	// dynamic cert reload means the server's now-renewed leaf and n2's
	// now-renewed client leaf are both presented with no listener
	// restart and no config rebuild.
	if err := dialWithConfigs(t, r.initRes.Store, preRotationSrv, preRotationCli); err != nil {
		t.Errorf("pre-rotation mTLS config broke after all nodes renewed: %v", err)
	}

	status, err = control.CARotationStatus(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("CARotationStatus (post-renew): %v", err)
	}
	if !status.Rotating || len(status.Pending) != 0 {
		t.Fatalf("status after all renewed = %+v, want Rotating with 0 pending", status)
	}

	if err := control.CompleteCARotation(ctx, r.initRes.Store); err != nil {
		t.Fatalf("CompleteCARotation: %v", err)
	}
	trust, _, err := control.LoadCATrust(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("LoadCATrust (post-complete): %v", err)
	}
	if trust.Outgoing != nil {
		t.Error("CompleteCARotation left an Outgoing CA behind")
	}

	bundle, err := control.LoadCATrustBundle(ctx, r.initRes.Store, nil)
	if err != nil {
		t.Fatalf("LoadCATrustBundle (post-complete): %v", err)
	}
	if err := bundle.VerifyNode(oldCACert, time.Now()); err == nil {
		t.Error("old CA still verifies as trusted after CompleteCARotation")
	}

	// And the pre-built config, dialed a third time, now correctly
	// fails: it was carrying n2's OLD (pre-renewal) leaf in the very
	// first dial only — but since dynamic reload always re-reads n2's
	// CURRENT disk cert, this actually still succeeds post-complete
	// too (n2 is fully renewed). Confirms retiring the old CA did not
	// break the already-renewed nodes.
	if err := dialWithConfigs(t, r.initRes.Store, preRotationSrv, preRotationCli); err != nil {
		t.Errorf("mTLS broke for an already-renewed node after CompleteCARotation: %v", err)
	}
}

// TestCompleteCARotationRefusesWhilePending covers CompleteCARotation's
// guard against retiring the outgoing CA early.
func TestCompleteCARotationRefusesWhilePending(t *testing.T) {
	r := newRig(t, "complete-guard")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := control.RotateCA(ctx, r.initRes.Store, r.initRes.Secret, time.Now()); err != nil {
		t.Fatalf("RotateCA: %v", err)
	}
	if err := control.CompleteCARotation(ctx, r.initRes.Store); !errors.Is(err, errors.KindConflict) {
		t.Errorf("CompleteCARotation with a pending node: err=%v, want conflict", err)
	}
}

// dialWithConfigs stands up a real listener with srvCfg and dials it
// with cliCfg, exercising a full mTLS handshake plus a forwarded RPC
// (dialInternalOK, serve_test.go) — not just pool/cert inspection.
func dialWithConfigs(t *testing.T, st *raftstore.Store, srvCfg, cliCfg *tls.Config) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(srvCfg)))
	pb.RegisterInternalStoreServiceServer(srv, raftstore.NewForwardServer(st))
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()
	return dialInternalOK(t, ln.Addr().String(), cliCfg)
}
