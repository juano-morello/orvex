package engineapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/juano-morello/orvex/internal/runtime/sessions"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

// The service token travels as "authorization: Bearer <token>".
const (
	authorizationKey = "authorization"
	bearerPrefix     = "Bearer "
)

// tokenAuth admits calls that carry the gateway's service token. It
// compares SHA-256 digests in constant time and keeps no plaintext.
type tokenAuth struct{ digest [sha256.Size]byte }

func newTokenAuth(token string) tokenAuth { return tokenAuth{digest: sha256.Sum256([]byte(token))} }

func (a tokenAuth) check(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(authorizationKey)
	if len(values) == 1 {
		if token, ok := strings.CutPrefix(values[0], bearerPrefix); ok {
			d := sha256.Sum256([]byte(token))
			if subtle.ConstantTimeCompare(d[:], a.digest[:]) == 1 {
				return nil
			}
		}
	}
	return status.Error(codes.Unauthenticated, "unauthenticated")
}

func (a tokenAuth) unary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	if err := a.check(ctx); err != nil {
		return nil, err
	}
	return next(ctx, req)
}

func (a tokenAuth) stream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	if err := a.check(ss.Context()); err != nil {
		return err
	}
	return next(srv, ss)
}

// serviceToken is the gateway's per-call credential.
type serviceToken string

func (t serviceToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{authorizationKey: bearerPrefix + string(t)}, nil
}

// RequireTransportSecurity is false: Dial accepts only loopback addresses.
func (serviceToken) RequireTransportSecurity() bool { return false }

// Client is a connection to the engine's EngineService.
type Client struct {
	orvexv1.EngineServiceClient
	conn *grpc.ClientConn
}

// Dial returns a client of the engine at addr, a host:port whose host is
// "localhost" or a loopback IP: the connection is plaintext, so the
// service token must not leave the machine. Every call carries token. Dial
// does not connect; the first call does.
func Dial(addr, token string) (*Client, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("engineapi: engine address: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("engineapi: the engine address must be a loopback host; the connection is plaintext")
	}
	if !sessions.ValidServiceToken(token) {
		return nil, fmt.Errorf("engineapi: the gateway token is not %d to 512 bytes of printable ASCII without spaces",
			sessions.MinServiceTokenBytes)
	}
	conn, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(serviceToken(token)),
	)
	if err != nil {
		return nil, fmt.Errorf("engineapi: %w", err)
	}
	return &Client{EngineServiceClient: orvexv1.NewEngineServiceClient(conn), conn: conn}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }
