#!/bin/sh
# fake-zfs.sh — file-backed zfs/zpool emulator for in-process exvol tests.
#
# Zvols are regular files, snapshots are copies, `send`/`receive` are
# byte copies of snapshot files (final state semantics: an incremental
# receive converges the zvol to the target snapshot, which is all the
# runtime can observe). One state dir per node via FAKE_ZFS_ROOT, so a
# harness "pool loss" is rm -rf of that dir — the zvol and its durable
# oplog ($ROOT/.oplogs, mountpoint=$ROOT) share the crash domain, like
# the real pool.
#
# Invocation: fake-zfs.sh <zfs|zpool> <subcommand args...>

MODE=$1
shift
ROOT=$FAKE_ZFS_ROOT

die() { echo "fake-zfs: $*" >&2; exit 1; }

# dev file for a dataset (slashes become nested dirs under $ROOT/dev)
devpath() { printf '%s/dev/%s' "$ROOT" "$1"; }
# snapshot file for ds@snap (flat: "/" sanitized to "_" so list is a
# plain mtime-sorted ls)
sanitize() { printf '%s' "$1" | tr '/' '_'; }
snappath() { printf '%s/snaps/%s' "$ROOT" "$(sanitize "$1")"; }

mkdir -p "$ROOT/dev" "$ROOT/snaps"

# last positional argument of a flag-laden invocation
lastname() { for a in "$@"; do :; done; printf '%s' "$a"; }

case "$MODE" in
zfs)
	case "$1" in
	create)
		shift
		size=
		while [ $# -gt 0 ]; do
			case "$1" in
			-V) size=$2; shift 2 ;;
			-b|-o) shift 2 ;;
			-*) shift ;;
			*) break ;;
			esac
		done
		[ -n "$size" ] || die "create without -V"
		[ $# -eq 1 ] || die "create: bad argv"
		f=$(devpath "$1")
		mkdir -p "$(dirname "$f")"
		truncate -s "$size" "$f" || die "truncate $f"
		;;

	destroy)
		shift
		name=$(lastname "$@")
		case "$name" in
		*@*)
			rm -f "$(snappath "$name")" || die "rm snap"
			;;
		*)
			rm -rf "$(devpath "$name")"
			for f in "$ROOT/snaps/$(sanitize "$name")"@*; do
				[ -e "$f" ] && rm -f "$f"
			done
			;;
		esac
		;;

	snapshot)
		name=$(lastname "$@")
		src=$(devpath "${name%%@*}")
		dst=$(snappath "$name")
		mkdir -p "$(dirname "$dst")"
		cp "$src" "$dst" || die "snapshot $name"
		;;

	list)
		# list -H -p -d 1 -t snapshot -o name <ds>  (oldest first)
		ds=$(lastname "$@")
		pfx=$(sanitize "$ds")
		for f in $(ls -1tr "$ROOT/snaps" 2>/dev/null); do
			case "$f" in
			"$pfx"@*) echo "$f" ;;
			esac
		done
		;;

	get)
		# get -H -o value mountpoint <ds>  (everything mounts at $ROOT)
		echo "$ROOT"
		;;

	send)
		shift
		name=$(lastname "$@")
		[ -f "$(snappath "$name")" ] || die "send: no snapshot $name"
		cat "$(snappath "$name")"
		printf 'FAKEZEND' # trailer: lets receive reject a stream cut short, like real zfs
		;;

	receive)
		# receive -F <dataset>
		# Model dataset replacement: write to a NEW file and rename it
		# over the zvol. A writer holding the old fd keeps reading the
		# OLD (pre-receive) inode — exactly like a real `zfs receive -F`
		# replacing the zvol dataset object under a pinned device node.
		shift
		name=$(lastname "$@")
		f=$(devpath "$name")
		mkdir -p "$(dirname "$f")"
		# Keep holes: a dense copy makes every later fake snapshot copy the whole volume.
		dd of="$f.tmp" bs=1M iflag=fullblock conv=sparse status=none || die "receive $name"
		if [ "$(tail -c 8 "$f.tmp")" != FAKEZEND ]; then
			rm -f "$f.tmp"
			die "receive $name: truncated stream"
		fi
		truncate -s -8 "$f.tmp" || die "receive $name"
		mv "$f.tmp" "$f" || die "receive $name"
		;;

	set)
		# set volsize=<n> <zvol>
		shift
		kv=$1
		shift
		n=${kv#volsize=}
		truncate -s "$n" "$(devpath "$(lastname "$@")")" || die "set volsize"
		;;

	*)
		die "zfs $1: not emulated"
		;;
	esac
	;;

zpool)
	case "$1" in
	status)
		# zpool status -H <pool>: one ONLINE vdev row + errors line
		pool=$(lastname "$@")
		printf '%s\tONLINE\t0\t0\t0\t0\t0\n' "$pool"
		echo "errors: No known data errors"
		;;
	*)
		die "zpool $1: not emulated"
		;;
	esac
	;;

*)
	die "mode must be zfs or zpool"
	;;
esac
