package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// write runs a write action in one transaction, idempotently: a mutation id the caller already used returns
// the stored response without running fn again (replayed is then true), even past the write's expiry, and a
// new write past its expiry is refused. fn fills out; the events it appends carry the actor and the mutation
// id.
func (d *Daemon) write(ctx context.Context, meta *v1.RequestMeta, procedure string, out proto.Message,
	fn func(t *store.Tx) error) (replayed bool, err error) {
	actor := auth.ActorFrom(ctx)
	if actor == nil {
		return false, connect.NewError(connect.CodeUnauthenticated, errors.New("no actor"))
	}
	mid := meta.GetMutationId()
	err = d.store.Write(ctx, actor, mid, func(t *store.Tx) error {
		if mid != "" {
			proc, resp, err := t.Mutation(actor.Id, mid)
			switch {
			case err == nil && proc != procedure:
				return reusedMutation(mid, proc)
			case err == nil:
				replayed = true
				return proto.Unmarshal(resp, out)
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		// Checked here, after the lookup (a retry of an applied write gets its result, however late) and
		// inside the transaction (a write that waited for the lock past its expiry doesn't run late).
		if err := expired(meta, t.Now()); err != nil {
			return err
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

// expired refuses a write past its meta's expiry.
func expired(meta *v1.RequestMeta, now time.Time) error {
	if exp := meta.GetExpiresAt(); exp != nil && now.After(exp.AsTime()) {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("request expired at %s", exp.AsTime().Format("15:04:05.000")))
	}
	return nil
}

// reusedMutation refuses a mutation id the caller already used for another procedure.
func reusedMutation(mid, proc string) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("mutation id %s was already used for %s", mid, proc))
}
