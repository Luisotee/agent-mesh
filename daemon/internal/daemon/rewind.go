package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// RewindAgent takes an agent's conversation back to before one of its prompts, as Claude Code's Esc Esc does for the
// conversation: a copy of its session cut before that prompt is written under a new id (`claude.CutSession`), the
// agent's session becomes the copy, and the agent is reopened on it by the usual reopen (the same agent, model,
// effort and worktree, `--resume <copy>`), or revived on it when it had stopped. The files, the commits and the mesh
// messages sent since stay as they are; the old session is kept on disk.
func (d *Daemon) RewindAgent(ctx context.Context, req *connect.Request[v1.RewindAgentRequest]) (*connect.Response[v1.RewindAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	found, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if !d.rt.adapters.HasCapability(found.Kind, claude.CapRewind) {
		return nil, failedPrecondition("rewind isn't available for %s: use its terminal (%s)", found.Kind, found.Name)
	}
	uid, _, ok := strings.Cut(req.Msg.EntryId, ":")
	if !ok || uid == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("entry_id is a transcript entry's id (`<uuid>:<block>`)"))
	}
	mu := d.agentLock(found.Id)
	mu.Lock()
	defer mu.Unlock()
	out := &v1.RewindAgentResponse{}
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRewindAgentProcedure, out, func(tx *store.Tx) error {
		a, err := tx.GetAgent(found.Id)
		if err != nil {
			return err
		}
		if isAdopted(a) {
			return failedPrecondition("%s is your own Claude Code session: rewind it in its terminal", a.Name)
		}
		if !stopped(a) && !atPrompt(a.State) {
			return failedPrecondition("%s is at work or waiting at a prompt in its terminal: interrupt it first, then rewind", a.Name)
		}
		if a.SessionId == "" || a.Worktree == "" {
			return failedPrecondition("%s has no conversation the daemon knows of to rewind", a.Name)
		}
		path := claude.TranscriptPath(d.agentEnv(a, ""), a.Worktree, a.SessionId)
		next := uuid.NewString()
		text, err := claude.CutSession(path, uid, next)
		switch {
		case errors.Is(err, claude.ErrNotAPrompt):
			return failedPrecondition("that entry is not one of %s's prompts in its current conversation", a.Name)
		case errors.Is(err, os.ErrNotExist):
			return failedPrecondition("%s's conversation file is not on this host (%s)", a.Name, path)
		case err != nil:
			return err
		}
		a.SessionId = next
		if stopped(a) {
			if _, err := d.revive(tx, a, reviveSpec{why: "rewound"}); err != nil {
				return err
			}
		} else {
			model, effort, deny := target(a)
			if _, err := requestReopen(tx, a, model, effort, deny, "rewound", "its conversation taken back to before a prompt"); err != nil {
				return err
			}
		}
		out.Text, out.SessionId = text, next
		return tx.PutAgent(a)
	}); err != nil {
		return nil, err
	}
	d.deliverSoon(found.Id)
	return connect.NewResponse(out), nil
}

// maxAttachment is the largest image a prompt may carry.
const maxAttachment = 10 << 20

// attachmentTypes are the images a prompt may carry, by extension, with the type their bytes must sniff as.
var attachmentTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// AddAttachment stores an image for a prompt in the agent's run folder (`attachments/<agent>/<id>.<ext>`), on the
// host, which may not be the machine the app runs on: the prompt names the path, and the CLI reads it with its file
// tool. The bytes must be the image their extension says.
func (d *Daemon) AddAttachment(ctx context.Context, req *connect.Request[v1.AddAttachmentRequest]) (*connect.Response[v1.AddAttachmentResponse], error) {
	actor := auth.ActorFrom(ctx)
	found, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	invalid := func(msg string) error { return connect.NewError(connect.CodeInvalidArgument, errors.New(msg)) }
	ext := strings.ToLower(filepath.Ext(req.Msg.Name))
	want, ok := attachmentTypes[ext]
	switch {
	case !ok:
		return nil, invalid("an attachment is a PNG, JPEG, GIF or WebP image (.png, .jpg, .jpeg, .gif, .webp)")
	case len(req.Msg.Data) == 0:
		return nil, invalid("the attachment is empty")
	case len(req.Msg.Data) > maxAttachment:
		return nil, invalid("an attachment is at most 10 MB")
	case http.DetectContentType(req.Msg.Data) != want:
		return nil, invalid("the file is not the " + strings.TrimPrefix(want, "image/") + " image its name says")
	}
	if ext == ".jpeg" {
		ext = ".jpg"
	}
	out := &v1.AddAttachmentResponse{}
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceAddAttachmentProcedure, out, func(tx *store.Tx) error {
		dir := filepath.Join(d.paths.RunDir(found.RunId), "attachments", found.Id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		path := filepath.Join(dir, ids.New("i", 10)+ext)
		if err := fsutil.WriteAtomic(path, req.Msg.Data, 0o600); err != nil {
			return err
		}
		out.Path = path
		return nil
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}
