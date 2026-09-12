package api

import (
	"context"
	"fmt"
	"log/slog"
	"os/user"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unaryRecovery converts handler panics into Internal errors instead of
// killing the connection.
func unaryRecovery(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in rpc", "method", info.FullMethod, "panic", fmt.Sprint(r))
				err = status.Error(codes.Internal, "internal panic")
			}
		}()
		return handler(ctx, req)
	}
}

func streamRecovery(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in stream rpc", "method", info.FullMethod, "panic", fmt.Sprint(r))
				err = status.Error(codes.Internal, "internal panic")
			}
		}()
		return handler(srv, ss)
	}
}

// unaryTimeout bounds unary handlers (default 30 s, per request config).
func unaryTimeout(def time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, ok := ctx.Deadline(); ok {
			return handler(ctx, req) // caller-supplied deadline wins
		}
		ctx, cancel := context.WithTimeout(ctx, def)
		defer cancel()
		return handler(ctx, req)
	}
}

func lookupGroup(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, err
	}
	return gid, nil
}
