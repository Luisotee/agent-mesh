package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Collecting a round's answers. There is no blocking CLI on the daemon: a caller ends its turn and everything
// rings at its next idle. So a collected ask (a round of reviewers) holds each member's `done` and `failed`
// (stored delivered and read: no doorbell) and, when the last member has answered or been settled, writes the
// answers into one file and sends the caller one `info`. A member that asks a question instead rings as usual
// and settles as "sent a question"; one that exits or goes silent settles with the alert its caller would have
// got, and a member still silent at the ask's deadline settles as not answered.

// collecting finds the open collected ask in which the agent is a pending member answering this caller.
func collecting(r store.Reader, memberID, callerID string) (*v1.Ask, int, error) {
	asks, err := r.OpenAsks()
	if err != nil {
		return nil, 0, err
	}
	for _, a := range asks {
		if !a.Collected || a.CallerId != callerID {
			continue
		}
		for i, m := range a.Members {
			if m.AgentId == memberID && m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
				return a, i, nil
			}
		}
	}
	return nil, 0, nil
}

func lineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// settleMember records what became of a member. When it was the last pending one, the ask finishes in the
// same transaction; the message the caller should be rung for (the one `info`) is returned.
func (d *Daemon) settleMember(tx *store.Tx, a *v1.Ask, idx int, status v1.AskMemberStatus, typ, line, text string) (*v1.Message, error) {
	m := a.Members[idx]
	m.Status, m.Type, m.Line, m.Text = status, typ, line, text
	return d.finishIfDone(tx, a)
}

// finishIfDone stores the ask and, with no member pending, finishes it: the answers file, one message to the
// caller.
func (d *Daemon) finishIfDone(tx *store.Tx, a *v1.Ask) (*v1.Message, error) {
	if a.Starting {
		return nil, tx.PutAsk(a) // members are still being started: their answers wait for the rest
	}
	for _, m := range a.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
			return nil, tx.PutAsk(a)
		}
	}
	a.Status = v1.AskStatus_ASK_STATUS_FINISHED
	a.FinishedAt = timestamppb.New(tx.Now())
	// A file that can't be written must not fail the member's report this runs in: the answers stay in the ask
	// (`mesh asks`) and the message says so.
	var fileErr error
	if a.Kind == v1.AskKind_ASK_KIND_PEERS {
		fileErr = writePeerAnswers(a)
		if err := recordPeerAnswers(tx, a); err != nil {
			return nil, err
		}
	} else {
		fileErr = writeAnswers(a)
	}
	if fileErr != nil {
		d.log.Warn("writing an ask's answers", "ask", a.Id, "err", fileErr)
		a.AnswersPath = ""
	}
	if err := tx.PutAsk(a); err != nil {
		return nil, err
	}
	caller, err := tx.GetAgent(a.CallerId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil // the caller is gone from the store: the file is all there is
		}
		return nil, err
	}
	var text string
	if a.Kind == v1.AskKind_ASK_KIND_PEERS {
		text = peerSummary(a)
	} else {
		text = d.summary(tx.Reader, a)
	}
	if fileErr != nil {
		text += "\n(the answers file could not be written: " + fileErr.Error() + "; read the answers with `mesh asks`)"
	}
	return tellTx(tx, caller, text)
}

// alertAsMember is alertParentTx for an agent that is a pending member of its parent's collected ask: an exit
// or silence settles the member with the alert's first line instead of ringing its caller (who is rung once,
// when the ask finishes). held says the alert was taken.
func (d *Daemon) alertAsMember(tx *store.Tx, member *v1.Agent, kind, text string) (ring *v1.Message, held bool, err error) {
	if kind != "exited" && kind != "silent" {
		return nil, false, nil // a limit or a quiet spell moves on by itself: the caller may want to act
	}
	a, idx, err := collecting(tx.Reader, member.Id, member.ParentId)
	if err != nil || a == nil {
		return nil, false, err
	}
	ring, err = d.settleMember(tx, a, idx, v1.AskMemberStatus_ASK_MEMBER_STATUS_SETTLED, kind, lineOf(text), "")
	return ring, true, err
}

// settleOverdueAsks is the pass over the open collected asks: a pending member whose agent was stopped (retired)
// without reporting settles as retired, and once the deadline has passed every one still pending settles as not
// answered, so a round whose member went silent still ends, with what came in.
func (d *Daemon) settleOverdueAsks(ctx context.Context) {
	r := d.store.Read(ctx)
	asks, err := r.OpenAsks()
	if err != nil {
		return
	}
	now := d.now()
	for _, a := range asks {
		if !a.Collected || ctx.Err() != nil {
			continue
		}
		var ring *v1.Message
		err := d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
			cur, err := tx.GetAsk(a.Id)
			if err != nil || cur.Status != v1.AskStatus_ASK_STATUS_OPEN {
				return err
			}
			due := cur.Deadline != nil && !now.Before(cur.Deadline.AsTime())
			changed := false
			if due && cur.Starting {
				cur.Starting, changed = false, true // its starter is gone: settle what it left
			}
			for _, m := range cur.Members {
				if m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
					continue
				}
				member, err := tx.GetAgent(m.AgentId)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				switch {
				case member == nil || stopped(member) && member.PendingReopen == nil:
					if member != nil {
						if rep, err := lastReport(tx.Reader, member); err != nil || rep != nil {
							// It reported (an answer held, or a question): the report settles it, not this pass.
							if err != nil {
								return err
							}
							continue
						}
					}
					m.Status, m.Type, m.Line = v1.AskMemberStatus_ASK_MEMBER_STATUS_SETTLED, "retired", "was stopped before it answered"
					changed = true
				case due:
					waited := cur.Deadline.AsTime().Sub(cur.CreatedAt.AsTime()).Round(time.Second)
					m.Status, m.Type, m.Line = v1.AskMemberStatus_ASK_MEMBER_STATUS_SETTLED, "timeout",
						fmt.Sprintf("not answered within %s (ask.timeout)", waited)
					changed = true
				}
			}
			if !changed {
				return nil
			}
			ring, err = d.finishIfDone(tx, cur)
			return err
		})
		if err != nil && ctx.Err() == nil {
			d.log.Warn("settling an ask", "ask", a.Id, "err", err)
		}
		if ring != nil && d.notify != nil {
			d.notify(ring.ToId)
		}
	}
}

// collectRef is a member's place in the round its report answers.
type collectRef struct {
	ask *v1.Ask
	idx int
}

// collectingRef is the round, if any, that a report from this agent to its parent answers.
func (d *Daemon) collectingRef(r store.Reader, self, to *v1.Agent) (*collectRef, error) {
	a, idx, err := collecting(r, self.Id, to.Id)
	if err != nil || a == nil {
		return nil, err
	}
	return &collectRef{ask: a, idx: idx}, nil
}

// settleReport settles a member by the report it sent: a `done` answers, a `failed`, `plan` or `question`
// settles it without an answer (a plan or question rings as usual, and its answer too).
func (d *Daemon) settleReport(tx *store.Tx, c *collectRef, typ v1.MessageType, body string) error {
	status, line := v1.AskMemberStatus_ASK_MEMBER_STATUS_SETTLED, ""
	name := strings.ToLower(strings.TrimPrefix(typ.String(), "MESSAGE_TYPE_"))
	switch typ {
	case v1.MessageType_MESSAGE_TYPE_DONE:
		status, line = v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED, "answered"
	case v1.MessageType_MESSAGE_TYPE_FAILED:
		line = "failed: " + lineOf(body)
	default:
		line = "sent a " + name + " instead of an answer: answer it with `mesh send " + c.ask.Members[c.idx].Name + " --type answer …`"
		body = ""
	}
	// The finishing message (if this was the last member) rings with the caller's other mail: SendMessage notifies it.
	_, err := d.settleMember(tx, c.ask, c.idx, status, name, line, body)
	return err
}
