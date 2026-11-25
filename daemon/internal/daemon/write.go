package daemon

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// write runs a write action in one transaction, idempotently: a mutation id the caller already used returns
// the stored response without running fn again (replayed is then true), and a write past its expiry is
// refused. fn fills out; the events it appends carry the actor and the mutation id.
func (d *Daemon) write(ctx context.Context, meta *v1.RequestMeta, procedure string, out proto.Message,
	fn func(t *store.Tx) error) (replayed bool, err error) {
	actor := auth.ActorFrom(ctx)
	if actor == nil {
		return false, connect.NewError(connect.CodeUnauthenticated, errors.New("no actor"))
	}
	if exp := meta.GetExpiresAt(); exp != nil && d.now().After(exp.AsTime()) {
		return false, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("request expired at %s", exp.AsTime().Format("15:04:05.000")))
	}
	mid := meta.GetMutationId()
	err = d.store.Write(ctx, actor, mid, func(t *store.Tx) error {
		if mid != "" {
			proc, resp, err := t.Mutation(actor.Id, mid)
			switch {
			case err == nil && proc != procedure:
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("mutation id %s was already used for %s", mid, proc))
			case err == nil:
				replayed = true
				return proto.Unmarshal(resp, out)
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		if err := fn(t); err != nil {
			return err
		}
		if mid == "" {
			return nil
		}
		b, err := proto.Marshal(out)
		if err != nil {
			return err
		}
		return t.PutMutation(actor.Id, mid, procedure, b)
	})
	return replayed, err
}
