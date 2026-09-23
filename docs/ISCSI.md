# iSCSI

How to deploy an `iscsi/target` block, connect to it, and what to expect from an initiator's
perspective when the node serving it fails. See `.plan/ARCHITECTURE.md` §9 (A24–A27) for the
design rationale; this document is the operator-facing companion.

## 1. The design, in one paragraph

`iscsi/target` exports one raw (no-filesystem) DRBD-backed volume as a single iSCSI LUN via LIO
(`targetcli-fb`). There is no real per-node ALUA and no multipath: a DRBD Secondary refuses to
even open a backstore, so the target can only ever run where the volume's DRBD primary is. The
block is therefore `SINGLETON`, colocated with the primary (the same guarantee `share/smb` already
relies on), and exposed at a stable external VIP (`EXPOSE_VIP`). Failover looks exactly like
`share/smb`'s own X2: the VIP moves to the node that takes over as primary, and the initiator's
own session recovery — not multipath path failover — reconnects to the same portal address.

## 2. Deploying a target

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: lun
  namespace: default
spec:
  type: iscsi/target
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: lun
      size: 10Gi
      replication: 3
      mountPath: /mnt/lun     # unused by this block type — LIO opens the raw device directly
      filesystem: none        # required: iscsi/target refuses a formatted storage entry
  config:
    port: 33260               # LIO's internal listen port — must differ from the exposed 3260
    iqn: iqn.2026-09.io.expanse:lun   # optional; see §4
  network:
    ports:
      - name: iscsi
        port: 3260
        target_port: 33260
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 33260
        period_seconds: 2
```

`config.port` must differ from the exposed portal port (3260): LIO listens on `config.port`
internally, and the VIP holder's own listen check on 3260 would otherwise collide with LIO the
instant the VIP moves onto that same node. `storage[].filesystem` must be `none` — this block type
never formats or mounts its bound volume (X5); the raw device is opened by LIO directly, and
`mountPath` is unused. Optional `config.chapUser`/`config.chapPassword` require CHAP
authentication at login; left unset, the target runs in demo mode, matching `share/smb`'s
`guestOk` posture.

Apply it the same way as any other block:

```sh
expanse ctl block apply -f lun.yaml
```

## 3. Connecting an initiator

**Do not use SendTargets discovery.** LIO's discovery response embeds its own internally-known
portal address, not the VIP the initiator actually dialed — the two never match, so a
discovery-driven login fails. Register the node directly against the VIP instead:

```sh
iscsiadm -m node -o new -T iqn.2026-09.io.expanse:lun -p <VIP>:3260
iscsiadm -m node -T iqn.2026-09.io.expanse:lun -p <VIP>:3260 --login
```

This is the supported production recipe, not a test workaround (`.plan/ARCHITECTURE.md` A27).
Validated against Linux `open-iscsi` only — ESXi, Windows Failover Cluster and other initiators are
not exercised by this project's VM tests and should be treated as unverified until someone does
(R3, `.plan/PHASE-04-TASKS.md`).

## 4. Identity: IQN and WWN

The target's IQN and the backstore's NAA WWN are each derived once, deterministically, from the
block instance's own namespace/name — the same way a volume ID is minted once. They are **not**
persisted anywhere and need no relocation on failover: because `SINGLETON` always runs the block's
one and only replica (index 0), the same instance name always hashes to the same identity,
regardless of which node currently hosts it. An initiator's already-established session and device
identity therefore never change across a failover, only reconnect. Override either explicitly with
`config.iqn`/`config.wwn` if a fixed, human-chosen identifier is needed instead.

## 5. Failover, from an initiator's perspective

When the node currently running the target is lost, the VIP moves to the surviving node that takes
over as DRBD primary, and LIO comes back up there with the same identity (§4). `open-iscsi`'s own
built-in session recovery reconnects to the same `VIP:3260` on its own — **no manual re-login is
required**. Measured on a 3-node test cluster against packaged defaults
(`nix/tests/iscsi-target-failover.nix`): ~64s from kill to the writer resuming, of which ~54s was
the VIP/DRBD-promotion re-convergence itself (not `open-iscsi`, which recovers quickly once the
portal is live again). No data is silently lost or corrupted across the gap — every acknowledged
write checksums correctly afterward, independently confirmed both from the initiator's own
read-back and from the new primary's own view of the raw device.

## 6. Persistent reservations

A SCSI-3 persistent reservation (`sg_persist`) registered by an initiator is kept **node-local** by
LIO and does **not** currently survive a failover: measured directly
(`nix/tests/iscsi-target-failover.nix`, `nix/tests/iscsi-vertical-slice.nix`), the new node's fresh
LIO instance reports zero registered keys after a failover that previously had one. This does not
block using the block type — most iSCSI clients that use PRs (e.g. cluster filesystems) already
handle a lost reservation as a fencing/reclaim event — but a workload wanting PR survival across
failover cannot rely on it today. Relocating PR state onto a second, filesystem-backed storage
entry (the same pattern `share/smb` uses to relocate its own state, D3) is a well-understood fix,
deferred until a workload actually needs it.
