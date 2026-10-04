package join

import (
	"context"
	"crypto/tls"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// removalAskTimeout bounds each peer's answer, so one hung peer cannot stall the rest.
const removalAskTimeout = 5 * time.Second

// AskRemoval asks the peers' join endpoints, in turn, whether nodeID was removed.
// It returns the revocation record, or nil once a peer confirms it was not removed.
func AskRemoval(ctx context.Context, peers []string, nodeID string, tlsCfg *tls.Config) ([]byte, error) {
	var failed []string
	for _, addr := range peers {
		resp, err := askPeer(ctx, addr, nodeID, tlsCfg)
		if err != nil {
			failed = append(failed, addr+": "+err.Error())
			continue
		}
		if !resp.GetRemoved() {
			return nil, nil
		}
		return resp.GetRevocation(), nil
	}
	return nil, errors.New(errors.KindUnavailable, "join.AskRemoval", "no peer could answer: "+strings.Join(failed, "; "))
}

func askPeer(ctx context.Context, addr, nodeID string, tlsCfg *tls.Config) (*pb.RemovalResponse, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, removalAskTimeout)
	defer cancel()
	return pb.NewJoinServiceClient(conn).Removal(ctx, &pb.RemovalRequest{NodeId: nodeID})
}
