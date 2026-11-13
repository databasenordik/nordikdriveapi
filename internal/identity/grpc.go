package identity

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	pb "nordik-drive-api/internal/identitypb"
	"nordik-drive-api/internal/workload"
)

type VerifyCaller func(context.Context, string, string, map[string]bool) (string, error)

func NewGRPC(service *Service, audience string) (*grpc.Server, error) {
	return newGRPC(service, audience, workload.VerifiedAccount)
}

func newGRPC(service *Service, audience string, verify VerifyCaller) (*grpc.Server, error) {
	if service == nil || service.DB == nil || audience == "" || len(service.Policy.Profiles) == 0 || verify == nil {
		return nil, errors.New("identity repository, audience and caller policy required")
	}
	interceptor := func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		accounts := service.Policy.Profiles
		if info.FullMethod == pb.Identity_BatchGetPrincipals_FullMethodName {
			accounts = service.Policy.Principals
		}
		md, _ := metadata.FromIncomingContext(ctx)
		tokens := md.Get("authorization")
		if len(tokens) != 1 || !strings.HasPrefix(tokens[0], "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "signed workload authorization required")
		}
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		account, err := verify(check, strings.TrimPrefix(tokens[0], "Bearer "), audience, accounts)
		cancel()
		if err != nil || account == "" || !accounts[account] {
			return nil, status.Error(codes.PermissionDenied, "identity caller denied")
		}
		return handler(context.WithValue(ctx, callerKey{}, account), request)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(interceptor), grpc.MaxRecvMsgSize(64*1024), grpc.MaxSendMsgSize(1024*1024))
	pb.RegisterIdentityServer(server, service)
	return server, nil
}
