package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"time"

	"github.com/spf13/cobra"
	pbproto "google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/quantity"
	exptransport "github.com/expanse/expanse/internal/storage/exvol/transport"
	pb "github.com/expanse/expanse/proto"
)

// volEntry is one volume's store view for the ctl commands (T15 §4.8).
type volEntry struct {
	id   string
	spec *pb.VolumeSpec
	st   *pb.VolumeStatus
}

// loadVolumes indexes all volumes by NAME via the agent socket. Reads
// the local FSM copy directly (Stale — §4.10.3's "degraded reads")
// rather than paying a linearizable barrier round trip: volume specs
// and placement rarely change moment to moment, `printInspect`'s one
// genuinely time-sensitive field (the state line) already prefers a
// live coordinator probe over anything from the store (see
// liveState), and a linearizable attempt costs a full
// raftstore.LeaderWaitTimeout (5s) whenever the raft control plane
// has no leader — exactly when `volume inspect`/`volume list` are
// needed most and have the least time to spare.
func loadVolumes(ctx context.Context, cl pb.NodeServiceClient) (map[string]*volEntry, error) {
	res, err := cl.ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: "/volumes/", Stale: true})
	if err != nil {
		return nil, err
	}
	out := map[string]*volEntry{}
	for _, e := range res.GetEntries() {
		id, suffix, ok := splitVolumeKey(e.GetKey())
		if !ok {
			continue
		}
		v := out[id]
		if v == nil {
			v = &volEntry{id: id}
			out[id] = v
		}
		switch suffix {
		case "spec":
			var s pb.VolumeSpec
			if pbproto.Unmarshal(e.GetValue(), &s) == nil {
				v.spec = &s
			}
		case "status":
			var s pb.VolumeStatus
			if pbproto.Unmarshal(e.GetValue(), &s) == nil {
				v.st = &s
			}
		}
	}
	// Key by name; fall back to the ID for half-written volumes.
	byName := map[string]*volEntry{}
	for _, v := range out {
		key := v.id
		if v.spec != nil && v.spec.GetName() != "" {
			key = v.spec.GetName()
		}
		byName[key] = v
	}
	return byName, nil
}

// resolveVol finds one volume by name (or ID).
func resolveVol(ctx context.Context, cl pb.NodeServiceClient, name string) (*volEntry, error) {
	vols, err := loadVolumes(ctx, cl)
	if err != nil {
		return nil, err
	}
	v, ok := vols[name]
	if !ok {
		return nil, fmt.Errorf("no such volume %q", name)
	}
	return v, nil
}

// putOp writes an operation request for the leader's controller /
// runtimes to consume (the ctl never touches the raft store directly).
func putOp(ctx context.Context, cl pb.NodeServiceClient, kind, volID string, val any) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = cl.PutKeyValue(ctx, &pb.PutKeyValueRequest{
		Key:   "/volumes/_ops/" + kind + "/" + volID,
		Value: raw,
	})
	return err
}

// meshAddrOf resolves a node ID to its exp0 overlay address by reading
// the node's published WireGuard peer record (§3: the node is the .1
// of its /24). The ctl dials replication transports directly for
// verify/diverged/liveState — these are data-path reads, not store
// writes. Reads Stale (local-FSM, no raft round trip): this record is
// written once at join and essentially never changes, so a stale copy
// is as good as a fresh one, and a linearizable attempt would cost a
// full raftstore.LeaderWaitTimeout whenever the raft control plane has
// no leader — exactly when liveState's raft-independent probe most
// needs to dial out quickly.
func meshAddrOf(ctx context.Context, cl pb.NodeServiceClient, nodeID string) (string, error) {
	key := "/nodes/" + nodeID + "/network.wgPublicKey"
	e, err := cl.GetKeyValue(ctx, &pb.GetKeyValueRequest{Key: key, Stale: true})
	if err != nil {
		return "", fmt.Errorf("no mesh record for %s: %w", nodeID, err)
	}
	var peer pb.WireGuardPeer
	if err := pbproto.Unmarshal(e.GetValue(), &peer); err != nil {
		return "", err
	}
	prefix, err := netip.ParsePrefix(peer.GetOverlayPrefix())
	if err != nil {
		return "", err
	}
	addr := prefix.Addr().As4()
	addr[3] = 1
	return netip.AddrFrom4(addr).String(), nil
}

// probeSeqs dials every replica and reports per-node last-seq + CRCs
// (T11's divergence-detection surface, reused — checksums come from
// the replicas' own op logs, never recomputed here).
func probeSeqs(ctx context.Context, cl pb.NodeServiceClient, volID string, nodes []string) (map[string]*pb.SeqQueryReply, []string) {
	replies := map[string]*pb.SeqQueryReply{}
	var down []string
	for _, n := range nodes {
		addr, err := meshAddrOf(ctx, cl, n)
		if err != nil {
			down = append(down, n)
			continue
		}
		conn, err := exptransport.Dial(ctx, fmt.Sprintf("%s:%d", addr, config.PortExvol))
		if err != nil {
			down = append(down, n)
			continue
		}
		rep, err := conn.QuerySeq(volID)
		conn.Close()
		if err != nil {
			down = append(down, n)
			continue
		}
		replies[n] = rep
	}
	return replies, down
}

// liveState probes the volume's current primary directly over the
// exvol replication transport (port 9440), bypassing the store/raft
// entirely — see runtime.querySeq's doc for why. Returns ok=false when
// the primary can't be reached or hasn't answered with live health
// (e.g. this build's primary predates the field, or the node named as
// primary no longer holds the role); the caller then falls back to
// the persisted status.State.
func liveState(ctx context.Context, cl pb.NodeServiceClient, v *volEntry) (pb.VolumeState, bool) {
	primary := v.st.GetPrimary()
	if cl == nil || primary == "" {
		return 0, false
	}
	// When the agent we're already talking to IS the primary, dial it
	// over loopback instead of resolving its own overlay address —
	// saves a hop, and works identically (the transport server binds
	// :9440, every interface including 127.0.0.1).
	addr := "127.0.0.1"
	if st, err := cl.GetStatus(ctx, &pb.GetStatusRequest{}); err != nil || st.GetNodeId() != primary {
		a, err := meshAddrOf(ctx, cl, primary)
		if err != nil {
			return 0, false
		}
		addr = a
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := exptransport.Dial(pctx, fmt.Sprintf("%s:%d", addr, config.PortExvol))
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	rep, err := conn.QuerySeq(v.id)
	if err != nil || !rep.GetIsPrimary() || rep.GetReplication() <= 0 {
		return 0, false
	}
	replication := int(rep.GetReplication())
	quorum := replication/2 + 1
	healthy := int(rep.GetHealthyReplicas())
	switch {
	case healthy >= replication:
		return pb.VolumeState_VOLUME_STATE_HEALTHY, true
	case healthy >= quorum:
		return pb.VolumeState_VOLUME_STATE_DEGRADED, true
	default:
		return pb.VolumeState_VOLUME_STATE_READONLY, true
	}
}

// crcAt extracts a replica's CRC for one seq (empty map → not durable).
func crcAt(r *pb.SeqQueryReply, seq uint64) (uint32, bool) {
	for _, op := range r.GetOps() {
		if op.GetSeq() == seq {
			return op.GetCrc32C(), true
		}
	}
	return 0, false
}

// newVolumeOpsCmds builds the T15 §4.8 subcommands.
func newVolumeOpsCmds(opts *ctlOpts) []*cobra.Command {
	var sizeStr, snapName, toNode, replica, chooseNode string
	var full bool

	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a volume (destroys every replica's zvol)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if err := putOp(ctx, cl, "delete", v.id, map[string]string{"target": args[0]}); err != nil {
					return err
				}
				fmt.Printf("volume %q delete requested\n", args[0])
				return nil
			})
		},
	}

	insp := &cobra.Command{
		Use:   "inspect <name>",
		Short: "Replicas, per-replica sequence + lag, resync progress (§4.8: answers \"is my data safe?\")",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if v.st == nil {
					return fmt.Errorf("volume %q has no status yet", args[0])
				}
				return printInspect(ctx, c.OutOrStdout(), cl, v)
			})
		},
	}

	resize := &cobra.Command{
		Use:   "resize <name>",
		Short: "Grow the volume (grow-only; shrink is refused by design)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			b, err := quantity.ParseBytes(sizeStr)
			if err != nil {
				return fmt.Errorf("--size: %w", err)
			}
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if v.spec != nil && b.N < int64(v.spec.GetSizeBytes()) {
					return fmt.Errorf("shrinking volumes is not supported (%s < %s)", sizeStr, humanBytes(v.spec.GetSizeBytes()))
				}
				if err := putOp(ctx, cl, "resize", v.id, map[string]any{"target": args[0], "sizeBytes": b.N}); err != nil {
					return err
				}
				fmt.Printf("volume %q resize to %s requested\n", args[0], sizeStr)
				return nil
			})
		},
	}

	snap := &cobra.Command{
		Use:   "snapshot <name>",
		Short: "Take a zvol snapshot on the primary",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if err := putOp(ctx, cl, "snapshot", v.id, map[string]string{"target": args[0], "name": snapName}); err != nil {
					return err
				}
				fmt.Printf("snapshot of %q requested (name %q)\n", args[0], snapName)
				return nil
			})
		},
	}

	restore := &cobra.Command{
		Use:   "restore <name>",
		Short: "Roll the volume back to a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if snapName == "" {
					return fmt.Errorf("--snapshot is required")
				}
				if err := putOp(ctx, cl, "restore", v.id, map[string]string{"target": args[0], "snapshot": snapName}); err != nil {
					return err
				}
				fmt.Printf("restore of %q to %q requested\n", args[0], snapName)
				return nil
			})
		},
	}

	move := &cobra.Command{
		Use:   "move-primary <name>",
		Short: "Move the primary to a node that already holds a replica",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if toNode == "" {
					return fmt.Errorf("--to <node> is required")
				}
				if err := putOp(ctx, cl, "move-primary", v.id, map[string]string{"target": args[0], "to": toNode}); err != nil {
					return err
				}
				fmt.Printf("primary move of %q to %s requested\n", args[0], toNode)
				return nil
			})
		},
	}

	resync := &cobra.Command{
		Use:   "resync <name>",
		Short: "Force-resync a stale replica from the primary (T12 path)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if replica == "" {
					return fmt.Errorf("--replica <node> is required")
				}
				if err := putOp(ctx, cl, "resync", v.id, map[string]any{"replica": replica, "full": full}); err != nil {
					return err
				}
				mode := "incremental"
				if full {
					mode = "FULL (spec-required WARNING: full zfs send)"
				}
				fmt.Printf("resync of %q to %s requested (%s)\n", args[0], replica, mode)
				return nil
			})
		},
	}

	verify := &cobra.Command{
		Use:   "verify <name>",
		Short: "Compare every replica's per-seq CRC32c; report mismatches",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				v, err := resolveVol(ctx, cl, args[0])
				if err != nil {
					return err
				}
				if v.st == nil {
					return fmt.Errorf("volume %q has no status yet", args[0])
				}
				var nodes []string
				for _, p := range v.st.GetPlacement() {
					nodes = append(nodes, p.GetNodeId())
				}
				replies, down := probeSeqs(ctx, cl, v.id, nodes)
				for _, n := range down {
					fmt.Printf("UNREACHABLE %s (lag unknown)\n", n)
				}
				if len(replies) < 2 {
					if len(replies) == 1 && len(down) == 0 {
						fmt.Println("OK single replica (nothing to cross-check)")
						return nil
					}
					return fmt.Errorf("cannot verify: too few replicas reachable (%d)", len(replies))
				}
				// Cross-check: every durable seq's CRC must agree across
				// replicas (T11's divergence rule, operator-facing form).
				seqs := map[uint64]bool{}
				for _, r := range replies {
					for _, op := range r.GetOps() {
						seqs[op.GetSeq()] = true
					}
				}
				ordered := make([]uint64, 0, len(seqs))
				for s := range seqs {
					ordered = append(ordered, s)
				}
				sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
				bad := 0
				for _, s := range ordered {
					var crc uint32
					var have uint32
					for n, r := range replies {
						c, ok := crcAt(r, s)
						if !ok {
							continue
						}
						if have == 0 {
							crc, have = c, 1
							continue
						}
						if c != crc {
							fmt.Printf("MISMATCH seq=%d: %s=%08x differs\n", s, n, c)
							bad++
						}
					}
				}
				for n, r := range replies {
					fmt.Printf("%-16s lastSeq=%d ops=%d\n", n, r.GetLastSeq(), len(r.GetOps()))
				}
				if bad > 0 {
					return fmt.Errorf("%d replica mismatches — data is NOT consistent", bad)
				}
				fmt.Println("OK all durable sequences agree across replicas")
				return nil
			})
		},
	}

	diverged := &cobra.Command{
		Use:   "diverged",
		Short: "List volumes waiting for §9 manual recovery; show branches",
		RunE: func(c *cobra.Command, args []string) error {
			return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
				vols, err := loadVolumes(ctx, cl)
				if err != nil {
					return err
				}
				names := make([]string, 0, len(vols))
				byName := map[string]*volEntry{}
				for n, v := range vols {
					if v.st != nil && v.st.GetState() == pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY {
						names = append(names, n)
						byName[n] = v
					}
				}
				if len(names) == 0 {
					fmt.Println("no diverged volumes")
					return nil
				}
				sort.Strings(names)
				for _, n := range names {
					v := byName[n]
					fmt.Printf("volume %s (%s) — NeedsManualRecovery\n", n, v.id)
					var nodes []string
					for _, p := range v.st.GetPlacement() {
						nodes = append(nodes, p.GetNodeId())
					}
					replies, down := probeSeqs(ctx, cl, v.id, nodes)
					for _, dn := range down {
						fmt.Printf("  UNREACHABLE %s\n", dn)
					}
					nodesByLast := map[uint64][]string{}
					for rn, r := range replies {
						fmt.Printf("  branch %s: lastSeq=%d ops=%d\n", rn, r.GetLastSeq(), len(r.GetOps()))
						nodesByLast[r.GetLastSeq()] = append(nodesByLast[r.GetLastSeq()], rn)
					}
					fmt.Println("  choose one branch with: expanse ctl volume diverged --choose <node>")
					_ = nodesByLast
				}
				return nil
			})
		},
	}
	// With --choose the command becomes an action on a specific
	// volume; without it, it lists the diverged volumes.
	diverged.RunE = func(c *cobra.Command, args []string) error {
		if chooseNode == "" {
			if len(args) != 0 {
				return fmt.Errorf("--choose is required to act on a volume; bare 'diverged' lists")
			}
			return divergedList(c, opts)
		}
		if len(args) != 1 {
			return fmt.Errorf("--choose requires a volume name argument")
		}
		return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
			v, err := resolveVol(ctx, cl, args[0])
			if err != nil {
				return err
			}
			if err := putOp(ctx, cl, "recover", v.id, map[string]string{"choose": chooseNode}); err != nil {
				return err
			}
			fmt.Printf("recovery of %q will adopt %s's branch (operator decision recorded; losing branch preserved)\n", args[0], chooseNode)
			return nil
		})
	}

	resize.Flags().StringVar(&sizeStr, "size", "", "new size (grow-only, e.g. 20Gi)")
	snap.Flags().StringVar(&snapName, "name", "", "snapshot name (default auto @resync-<seq>)")
	restore.Flags().StringVar(&snapName, "snapshot", "", "snapshot to restore")
	move.Flags().StringVar(&toNode, "to", "", "destination node (must hold a replica)")
	resync.Flags().StringVar(&replica, "replica", "", "replica node to resync")
	resync.Flags().BoolVar(&full, "full", false, "force a full (non-incremental) send")
	diverged.Flags().StringVar(&chooseNode, "choose", "", "adopt <node>'s branch as truth (node must hold a replica)")
	diverged.Use = "diverged [name]"
	diverged.Args = cobra.MaximumNArgs(1)

	return []*cobra.Command{del, insp, resize, snap, restore, move, resync, verify, diverged}
}

// printInspect renders §4.8's `volume inspect`: the per-replica
// sequence + lag view that answers "is my data safe?". The state line
// prefers a live probe of the primary (liveState) over the persisted
// status.State — the latter can only ever be as fresh as the
// cluster's raft control plane last had write quorum to record it,
// which is exactly unavailable in the scenario ("state" during a
// write-quorum loss) an operator most needs this command to answer.
func printInspect(ctx context.Context, w io.Writer, cl pb.NodeServiceClient, v *volEntry) error {
	st := v.st.GetState()
	if live, ok := liveState(ctx, cl, v); ok {
		st = live
	}
	fmt.Fprintf(w, "volume %s (%s)\n", nameOf(v), v.id)
	fmt.Fprintf(w, "  state:    %s\n", stateStr(st))
	fmt.Fprintf(w, "  primary:  %s\n", v.st.GetPrimary())
	fmt.Fprintf(w, "  size:     %s\n", humanBytes(v.spec.GetSizeBytes()))
	fmt.Fprintf(w, "  sequence: %d (last acked)\n", v.st.GetSequence())
	fmt.Fprintf(w, "  %-16s %-12s %-10s %-8s %s\n", "REPLICA", "ROLE", "SEQ", "LAG", "LAST SEEN")
	for _, p := range v.st.GetPlacement() {
		lag := uint64(0)
		if v.st.GetSequence() >= p.GetSequence() {
			lag = v.st.GetSequence() - p.GetSequence()
		}
		seen := "never"
		if p.GetLastSeenUnixNano() > 0 {
			seen = relTime(p.GetLastSeenUnixNano())
		}
		fmt.Fprintf(w, "  %-16s %-12s %-10d %-8d %s\n",
			p.GetNodeId(), roleStr(p.GetRole()), p.GetSequence(), lag, seen)
	}
	return nil
}

// divergedList is the listing form of `volume diverged`.
func divergedList(c *cobra.Command, opts *ctlOpts) error {
	return withClient(c, opts, func(ctx context.Context, cl pb.NodeServiceClient) error {
		w := c.OutOrStdout()
		vols, err := loadVolumes(ctx, cl)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(vols))
		for n, v := range vols {
			if v.st != nil && v.st.GetState() == pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			fmt.Fprintln(w, "no diverged volumes")
			return nil
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(w, "volume %s — NeedsManualRecovery (pick a branch: diverged <name> --choose <node>)\n", n)
		}
		return nil
	})
}

// stateStr renders a pb.VolumeState the way the Go enum prints
// ("Healthy", "NeedsManualRecovery", ...).
// stateStr renders a VolumeState exactly as internal/storage/model.go's
// VolumeState string constants spell it — an explicit table, not a
// per-underscore title-caser: VOLUME_STATE_READONLY is one compound
// word with no separating underscore, so naive title-casing produced
// "Readonly" instead of "ReadOnly" (silently unnoticed until this
// state became reachable — vol-degraded.nix, G6.12).
func stateStr(vs pb.VolumeState) string {
	switch vs {
	case pb.VolumeState_VOLUME_STATE_CREATING:
		return "Creating"
	case pb.VolumeState_VOLUME_STATE_HEALTHY:
		return "Healthy"
	case pb.VolumeState_VOLUME_STATE_DEGRADED:
		return "Degraded"
	case pb.VolumeState_VOLUME_STATE_READONLY:
		return "ReadOnly"
	case pb.VolumeState_VOLUME_STATE_RESYNCING:
		return "Resyncing"
	case pb.VolumeState_VOLUME_STATE_FAILED:
		return "Failed"
	case pb.VolumeState_VOLUME_STATE_DELETING:
		return "Deleting"
	case pb.VolumeState_VOLUME_STATE_NEEDS_MANUAL_RECOVERY:
		return "NeedsManualRecovery"
	default:
		return "Unspecified"
	}
}

func nameOf(v *volEntry) string {
	if v.spec != nil && v.spec.GetName() != "" {
		return v.spec.GetName()
	}
	return v.id
}

func roleStr(r pb.ReplicaRole) string {
	switch r {
	case pb.ReplicaRole_REPLICA_ROLE_PRIMARY:
		return "primary"
	case pb.ReplicaRole_REPLICA_ROLE_SECONDARY:
		return "secondary"
	case pb.ReplicaRole_REPLICA_ROLE_RESYNCING:
		return "resyncing"
	case pb.ReplicaRole_REPLICA_ROLE_STALE:
		return "stale"
	}
	return "?"
}

// humanBytes renders a byte count in the usual units.
func humanBytes(n uint64) string {
	b := quantity.Bytes{N: int64(n)}
	return b.String()
}

// relTime formats a UnixNano timestamp as a coarse age.
func relTime(un int64) string {
	d := time.Since(time.Unix(0, un))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	default:
		return d.Round(time.Hour).String()
	}
}
