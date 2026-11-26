package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// CreateWorktree creates a standalone run worktree with no agent in it (e.g. for integration), recorded in the
// run so StopRun cleans it up like a worker's and CheckConflicts checks its branch.
func (d *Daemon) CreateWorktree(ctx context.Context, req *connect.Request[v1.CreateWorktreeRequest]) (*connect.Response[v1.CreateWorktreeResponse], error) {
	actor := auth.ActorFrom(ctx)
	m := req.Msg
	if !slugRE.MatchString(m.Slug) {
		return nil, invalid("slug %q: lowercase letters, digits and dashes", m.Slug)
	}
	runID := m.RunId
	if IsAgent(actor.Class) {
		if runID == "" {
			runID = actor.RunId
		} else if runID != actor.RunId {
			return nil, denied("an agent creates worktrees only in its own run")
		}
	}
	if runID == "" {
		return nil, invalid("no run_id given")
	}
	r := d.store.Read(ctx)
	run, err := r.GetRun(runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no run %q", runID)
	}
	if err != nil {
		return nil, err
	}
	if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		return nil, failedPrecondition("run %s is closed", runID)
	}
	if mid := m.Meta.GetMutationId(); mid != "" {
		if _, resp, err := r.Mutation(actor.Id, mid); err == nil {
			out := &v1.CreateWorktreeResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		}
	}
	if strings.TrimSpace(m.Repo) == "" {
		return nil, invalid("no repo given")
	}
	repoArg, err := filepath.Abs(m.Repo)
	if err != nil {
		return nil, invalid("repo: %v", err)
	}
	top, err := git(ctx, repoArg, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, invalid("%s is not a git repository: %v", m.Repo, err)
	}
	if _, err := r.GetWorktree(runID, m.Slug); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("run %s already has worktree %q", runID, m.Slug))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	base := m.Base
	if base == "" {
		base = defaultBase(ctx, top)
	}
	if strings.HasPrefix(base, "-") {
		return nil, invalid("base %q looks like an option", base)
	}
	baseCommit, err := git(ctx, top, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return nil, failedPrecondition("bad base %q: %v", base, err)
	}
	if err := d.ensureRunDir(runID); err != nil {
		return nil, err
	}
	branch := "mesh/" + runID + "/" + m.Slug
	wtPath := filepath.Join(d.worktrees(), runID, filepath.Base(top)+"-"+m.Slug)
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o700); err != nil {
		return nil, err
	}
	if _, err := git(ctx, top, "worktree", "add", "-b", branch, wtPath, baseCommit); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	cleanup := func() {
		_, _ = git(context.Background(), top, "worktree", "remove", "--force", wtPath)
		_, _ = git(context.Background(), top, "branch", "-D", branch)
	}

	out := &v1.CreateWorktreeResponse{}
	replayed, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceCreateWorktreeProcedure, out, func(tx *store.Tx) error {
		if run, err := tx.GetRun(runID); err != nil {
			return err
		} else if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
			return failedPrecondition("run %s is closed", runID)
		}
		wt := &v1.Worktree{
			Id:         ids.New("w", 10),
			RunId:      runID,
			Slug:       m.Slug,
			Repo:       top,
			Path:       wtPath,
			Branch:     branch,
			BaseCommit: baseCommit,
			CreatedAt:  timestamppb.New(tx.Now()),
		}
		if err := tx.PutWorktree(wt); err != nil {
			if errors.Is(err, store.ErrExists) {
				return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("run %s already has worktree %q", runID, m.Slug))
			}
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{
			RunId: runID,
			Payload: &v1.Event_WorktreeCreated{
				WorktreeCreated: &v1.WorktreeCreated{Worktree: wt},
			},
		}); err != nil {
			return err
		}
		out.Worktree = wt
		return nil
	})
	if err != nil {
		if !replayed {
			cleanup()
		}
		return nil, err
	}
	return connect.NewResponse(out), nil
}
