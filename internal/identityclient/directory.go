// Package identityclient is the owner API used outside Identity. A failed call
// is an explicit dependency error; it never falls back to querying user tables.
package identityclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	pb "nordik-drive-api/internal/identitypb"
	"nordik-drive-api/internal/workload"
)

type Profile struct {
	ID                         int64
	Firstname, Lastname, Email string
}
type Principal struct {
	ID          int64
	Role        string
	Communities []string
}
type Directory interface {
	Profiles(context.Context, []int64, bool) (map[int64]Profile, error)
	Principals(context.Context, []int64) (map[int64]Principal, error)
	Search(context.Context, string, string, string) ([]int64, error)
}

type Client struct{ RPC pb.IdentityClient }

func Configured(ctx context.Context) (Directory, func(), error) {
	address := os.Getenv("IDENTITY_URL")
	if address == "" {
		return nil, func() {}, nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, nil, errors.New("IDENTITY_URL must be an HTTPS origin")
	}
	tokens, err := workload.Source(ctx, address)
	if err != nil {
		return nil, nil, err
	}
	target := u.Host
	if u.Port() == "" {
		target = net.JoinHostPort(u.Hostname(), "443")
	}
	connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})), grpc.WithPerRPCCredentials(workload.SignedCredentials{Source: tokens}), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1024*1024)))
	if err != nil {
		return nil, nil, errors.New("identity client initialization failed")
	}
	return &Client{RPC: pb.NewIdentityClient(connection)}, func() { _ = connection.Close() }, nil
}

func batches(ids []int64) ([][]int64, error) {
	seen := map[int64]bool{}
	unique := []int64{}
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("invalid identity ID")
		}
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	out := [][]int64{}
	for len(unique) > 0 {
		n := min(100, len(unique))
		out = append(out, unique[:n])
		unique = unique[n:]
	}
	return out, nil
}

func (c *Client) Profiles(ctx context.Context, ids []int64, email bool) (map[int64]Profile, error) {
	chunks, err := batches(ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]Profile{}
	for _, chunk := range chunks {
		call, cancel := context.WithTimeout(ctx, 8*time.Second)
		response, err := c.RPC.BatchGetProfiles(call, &pb.BatchGetProfilesRequest{Ids: chunk, IncludeEmail: email})
		cancel()
		if err != nil {
			return nil, errors.New("identity profiles unavailable")
		}
		if response == nil {
			return nil, errors.New("invalid identity profile response")
		}
		remaining := map[int64]bool{}
		for _, id := range chunk {
			remaining[id] = true
		}
		for _, p := range response.Profiles {
			if p == nil || !remaining[p.Id] || (!email && p.Email != "") {
				return nil, errors.New("invalid identity profile response")
			}
			delete(remaining, p.Id)
			out[p.Id] = Profile{ID: p.Id, Firstname: p.Firstname, Lastname: p.Lastname, Email: p.Email}
		}
		for _, id := range response.MissingIds {
			if !remaining[id] {
				return nil, errors.New("invalid identity missing IDs")
			}
			delete(remaining, id)
		}
		if len(remaining) > 0 {
			return nil, errors.New("incomplete identity profile response")
		}
	}
	return out, nil
}

func (c *Client) Principals(ctx context.Context, ids []int64) (map[int64]Principal, error) {
	chunks, err := batches(ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]Principal{}
	for _, chunk := range chunks {
		call, cancel := context.WithTimeout(ctx, 8*time.Second)
		response, err := c.RPC.BatchGetPrincipals(call, &pb.BatchGetPrincipalsRequest{Ids: chunk})
		cancel()
		if err != nil {
			return nil, errors.New("identity principals unavailable")
		}
		if response == nil {
			return nil, errors.New("invalid identity principal response")
		}
		remaining := map[int64]bool{}
		for _, id := range chunk {
			remaining[id] = true
		}
		for _, p := range response.Principals {
			if p == nil || !remaining[p.Id] {
				return nil, errors.New("invalid identity principal response")
			}
			delete(remaining, p.Id)
			out[p.Id] = Principal{ID: p.Id, Role: p.Role, Communities: append([]string(nil), p.Communities...)}
		}
		for _, id := range response.MissingIds {
			if !remaining[id] {
				return nil, errors.New("invalid identity missing IDs")
			}
			delete(remaining, id)
		}
		if len(remaining) > 0 {
			return nil, errors.New("incomplete identity principal response")
		}
	}
	return out, nil
}

func (c *Client) Search(ctx context.Context, query, mode, role string) ([]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ids := []int64{}
	after := int64(0)
	for {
		response, err := c.RPC.SearchProfiles(ctx, &pb.SearchProfilesRequest{Query: query, MatchMode: mode, Role: role, AfterId: after, Limit: 100})
		if err != nil {
			return nil, errors.New("identity search unavailable")
		}
		if response == nil || len(response.Ids) > 100 {
			return nil, errors.New("invalid identity search response")
		}
		previous := after
		for _, id := range response.Ids {
			if id <= previous {
				return nil, errors.New("unordered identity search response")
			}
			previous = id
		}
		ids = append(ids, response.Ids...)
		if response.NextAfterId == 0 {
			return ids, nil
		}
		if len(response.Ids) == 0 || response.NextAfterId != previous {
			return nil, errors.New("invalid identity pagination")
		}
		after = response.NextAfterId
	}
}
