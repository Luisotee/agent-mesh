// Package auth turns a request's bearer token into its actor and checks the actor's class against the
// registry. The actor is never read from the request body.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
)

// TokenPrefix starts every token the daemon issues, so a leaked one is recognisable.
const TokenPrefix = "amt_"

// NewToken returns a fresh secret token.
func NewToken() string { return ids.Secret(TokenPrefix) }

// Hash is how a token is stored and looked up.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrUnknownToken is what a Resolver returns for a token it doesn't know or that was revoked.
var ErrUnknownToken = errors.New("unknown or revoked token")

// Resolver maps a token's hash to its actor.
type Resolver interface {
	ResolveToken(ctx context.Context, hash string) (*v1.Actor, error)
}

type actorKey struct{}

// WithActor returns ctx carrying the actor.
func WithActor(ctx context.Context, a *v1.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor the interceptor resolved; nil outside an authenticated call.
func ActorFrom(ctx context.Context) *v1.Actor {
	a, _ := ctx.Value(actorKey{}).(*v1.Actor)
	return a
}

// BearerToken returns the token of an Authorization header, or "".
func BearerToken(h http.Header) string {
	v := h.Get("Authorization")
	if tok, ok := strings.CutPrefix(v, "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	return ""
}

// Interceptor authenticates every call and authorizes it by the registry.
type Interceptor struct {
	reg *registry.Registry
	res Resolver
}

// NewInterceptor returns the interceptor for a registry.
func NewInterceptor(reg *registry.Registry, res Resolver) *Interceptor {
	return &Interceptor{reg: reg, res: res}
}

func (i *Interceptor) authorize(ctx context.Context, procedure string, h http.Header) (context.Context, error) {
	tok := BearerToken(h)
	if tok == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	}
	actor, err := i.res.ResolveToken(ctx, Hash(tok))
	if errors.Is(err, ErrUnknownToken) {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if i.reg.Get(procedure) == nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New(procedure+" is not in the registry"))
	}
	if !i.reg.Allowed(procedure, actor.Class) {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New(actor.Class.String()+" may not call "+procedure))
	}
	return WithActor(ctx, actor), nil
}

// WrapUnary implements connect.Interceptor.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := i.authorize(ctx, req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.authorize(ctx, conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// RefuseBrowsers answers 403 to any request a browser made (it carries an Origin or Sec-Fetch-Site header),
// so a web page can't drive the local API even when it can reach the port.
func RefuseBrowsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "browser requests are refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientToken is a client interceptor that sends a bearer token with every call.
type ClientToken string

// WrapUnary implements connect.Interceptor.
func (t ClientToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(t))
		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (t ClientToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(t))
		return conn
	}
}

// WrapStreamingHandler implements connect.Interceptor.
func (t ClientToken) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
