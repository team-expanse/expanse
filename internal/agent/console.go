package agent

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/expanse/expanse/internal/blocks/vm/console"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/config"
	pb "github.com/expanse/expanse/proto"
)

// consoleService serves the consoles of VMs on this node and forwards the rest to their nodes.
func (a *Agent) consoleService(ctx context.Context) *console.Service {
	svc := &console.Service{NodeID: a.cfg.NodeID, Locate: console.Locator(a.blocks.Get), Done: ctx.Done()}
	if a.ctl == nil {
		return svc
	}
	tlsCfg, err := control.InternalClientTLS(ctx, a.ctl.store, a.ctl.ca, a.ctl.dataDir)
	if err != nil {
		a.logger.Warn("VM consoles on other nodes are unreachable from here", "err", err)
		return svc
	}
	rs := a.ctl.store
	svc.Dial = (&console.NodeDialer{
		Addr: func(id string) (string, error) {
			raftAddr, err := rs.ServerAddress(id)
			if err != nil {
				return "", err
			}
			if raftAddr == "" {
				return "", fmt.Errorf("%s is not a cluster member", id)
			}
			host, _, err := net.SplitHostPort(raftAddr)
			if err != nil {
				return "", err
			}
			return net.JoinHostPort(host, strconv.Itoa(config.PortAPI)), nil
		},
		Creds: credentials.NewTLS(tlsCfg),
	}).Dial
	return svc
}

// registerConsole adds svc to a gRPC server; a nil svc registers nothing.
func registerConsole(svc *console.Service) func(grpc.ServiceRegistrar) {
	return func(r grpc.ServiceRegistrar) {
		if svc != nil {
			pb.RegisterConsoleServiceServer(r, svc)
		}
	}
}
