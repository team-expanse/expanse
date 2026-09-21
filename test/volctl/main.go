// Command volctl drives the allocator and the volume runtime from the VM tests,
// printing JSON. It is test tooling and is not shipped.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
	"github.com/expanse/expanse/internal/storage/volume"
	"github.com/expanse/expanse/internal/store/boltstore"
)

type command func(ctx context.Context, args []string, out io.Writer) error

var commands = map[string]command{
	"alloc":     alloc,
	"assign":    assign,
	"retire":    retire,
	"ack":       ack,
	"show":      show,
	"desired":   desired,
	"reconcile": reconcile,
	"hold":      hold,
	"observe":   observe,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: volctl alloc|assign|retire|ack|show|desired|reconcile|hold|observe [flags]")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](context.Background(), os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "volctl:", err)
		os.Exit(1)
	}
}

// allocFlags are the flags every allocator command shares.
type allocFlags struct {
	fs             *flag.FlagSet
	db, name, host string
	id             int
}

func newAllocFlags(name string) *allocFlags {
	f := &allocFlags{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	f.fs.StringVar(&f.db, "db", "", "allocator database file")
	f.fs.StringVar(&f.name, "name", "", "resource name")
	f.fs.StringVar(&f.host, "host", "", "host name")
	f.fs.IntVar(&f.id, "id", -1, "node-id")
	return f
}

func (f *allocFlags) open(args []string) (*drbd.Allocator, func(), error) {
	if err := f.fs.Parse(args); err != nil {
		return nil, nil, err
	}
	st, err := boltstore.New(f.db)
	if err != nil {
		return nil, nil, err
	}
	return drbd.NewAllocator(st, drbd.DefaultMinors, drbd.DefaultPorts), func() { _ = st.Close() }, nil
}

func printJSON(out io.Writer, v any) error { return json.NewEncoder(out).Encode(v) }

func alloc(ctx context.Context, args []string, out io.Writer) error {
	f := newAllocFlags("alloc")
	hosts := f.fs.String("hosts", "", "comma-separated hosts to give node-ids")
	a, done, err := f.open(args)
	if err != nil {
		return err
	}
	defer done()
	if _, err := a.Allocate(ctx, f.name); err != nil {
		return err
	}
	for _, h := range strings.Split(*hosts, ",") {
		if _, err := a.AssignNodeID(ctx, f.name, h); err != nil {
			return err
		}
	}
	return printAllocation(ctx, a, f.name, out)
}

func assign(ctx context.Context, args []string, out io.Writer) error {
	f := newAllocFlags("assign")
	a, done, err := f.open(args)
	if err != nil {
		return err
	}
	defer done()
	id, err := a.AssignNodeID(ctx, f.name, f.host)
	fmt.Fprintln(out, id)
	return err
}

func retire(ctx context.Context, args []string, out io.Writer) error {
	f := newAllocFlags("retire")
	a, done, err := f.open(args)
	if err != nil {
		return err
	}
	defer done()
	id, err := a.RetireNode(ctx, f.name, f.host)
	fmt.Fprintln(out, id)
	return err
}

func ack(ctx context.Context, args []string, _ io.Writer) error {
	f := newAllocFlags("ack")
	a, done, err := f.open(args)
	if err != nil {
		return err
	}
	defer done()
	return a.AckForgotten(ctx, f.name, f.id, f.host)
}

func show(ctx context.Context, args []string, out io.Writer) error {
	f := newAllocFlags("show")
	a, done, err := f.open(args)
	if err != nil {
		return err
	}
	defer done()
	return printAllocation(ctx, a, f.name, out)
}

func printAllocation(ctx context.Context, a *drbd.Allocator, name string, out io.Writer) error {
	al, err := a.Get(ctx, name)
	if err != nil {
		return err
	}
	return printJSON(out, al)
}

func parseAddrs(s string) (map[string]netip.Addr, error) {
	addrs := map[string]netip.Addr{}
	for _, kv := range strings.Split(s, ",") {
		host, ip, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, errors.New("addrs wants host=ip,host=ip")
		}
		a, err := netip.ParseAddr(ip)
		if err != nil {
			return nil, err
		}
		addrs[host] = a
	}
	return addrs, nil
}

func desired(ctx context.Context, args []string, out io.Writer) error {
	f := newAllocFlags("desired")
	self := f.fs.String("self", "", "this node")
	size := f.fs.Uint64("size", 0, "usable bytes")
	thin := f.fs.Bool("thin", true, "thin provisioning")
	addrFlag := f.fs.String("addrs", "", "host=ip,host=ip")
	a, done, err := f.open(args)
	if err != nil {
		return err
	}
	defer done()
	addrs, err := parseAddrs(*addrFlag)
	if err != nil {
		return err
	}
	al, err := a.Get(ctx, f.name)
	if err != nil {
		return err
	}
	d, err := volume.FromAllocation(al, *self, *size, *thin, addrs)
	if err != nil {
		return err
	}
	return printJSON(out, d)
}

func reconcile(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	file := fs.String("desired", "", "desired-state JSON file")
	vg := fs.String("vg", "vg0", "volume group")
	pool := fs.String("pool", "pool", "thin pool")
	dir := fs.String("confdir", "/etc/drbd.d", "DRBD config directory")
	sb := fs.String("splitbrain", "", "split-brain handler")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var d volume.Desired
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	rt := &volume.Runtime{LVM: lvm.New(), DRBD: drbd.New(), VG: *vg, Pool: *pool, ConfigDir: *dir, SplitBrainCmd: *sb}
	res, err := rt.Reconcile(ctx, d)
	if perr := printJSON(out, res); perr != nil {
		return perr
	}
	return err
}

func observe(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	name := fs.String("name", "", "resource name")
	self := fs.String("self", "", "this node")
	ids := fs.String("members", "", "host=node-id,host=node-id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	members, err := parseMembers(*ids)
	if err != nil {
		return err
	}
	st, err := drbd.New().Status(ctx, *name)
	if err != nil {
		return err
	}
	return printJSON(out, volume.Observe(st, *self, members))
}

func parseMembers(s string) ([]drbd.Member, error) {
	var members []drbd.Member
	for _, kv := range strings.Split(s, ",") {
		host, id, ok := strings.Cut(kv, "=")
		n, err := strconv.Atoi(id)
		if !ok || err != nil {
			return nil, errors.New("members wants host=node-id,host=node-id")
		}
		members = append(members, drbd.Member{Host: host, NodeID: n})
	}
	return members, nil
}
