package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestAPromptLeavesTheQueueOnceTypedOrWithdrawnNeverBoth(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(fixed.Add(d)) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Write(ctx, nil, "", func(tx *Tx) error {
		for i, id := range []string{"p2", "p1", "p3"} {
			agent := "a1"
			if id == "p3" {
				agent = "a2"
			}
			// p2 is queued after p1 though inserted first: the queue's order is created_at's.
			if err := tx.QueuePrompt(&v1.QueuedPrompt{Id: id, AgentId: agent, Text: "say " + id, CreatedAt: at(time.Duration(2-i) * time.Second)}); err != nil {
				return err
			}
		}
		return nil
	}))
	r := s.Read(ctx)
	q, err := r.QueuedPrompts("a1")
	must(err)
	if len(q) != 2 || q[0].Id != "p1" || q[1].Id != "p2" {
		t.Fatalf("queue of a1 = %v", q)
	}
	agents, err := r.AgentsWithQueuedPrompts()
	must(err)
	if len(agents) != 2 {
		t.Errorf("agents with queued prompts = %v", agents)
	}

	// Claimed for typing: a withdrawal after it finds nothing to take back, and a second claim neither.
	must(s.Write(ctx, nil, "", func(tx *Tx) error {
		p, err := tx.ClaimPrompt("a1", "p1")
		if err != nil || p.Text != "say p1" {
			t.Errorf("claim = %v, %v", p, err)
		}
		if _, err := tx.WithdrawPrompt("a1", "p1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("withdrawing a claimed prompt = %v", err)
		}
		if _, err := tx.ClaimPrompt("a1", "p1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("claiming twice = %v", err)
		}
		// Another agent's prompt can't be taken by naming it under this one.
		if _, err := tx.WithdrawPrompt("a1", "p3"); !errors.Is(err, ErrNotFound) {
			t.Errorf("withdrawing another agent's prompt = %v", err)
		}
		return nil
	}))
	// Withdrawn: it can't be claimed after.
	must(s.Write(ctx, nil, "", func(tx *Tx) error {
		if _, err := tx.WithdrawPrompt("a1", "p2"); err != nil {
			t.Errorf("withdraw = %v", err)
		}
		if _, err := tx.ClaimPrompt("a1", "p2"); !errors.Is(err, ErrNotFound) {
			t.Errorf("claiming a withdrawn prompt = %v", err)
		}
		return nil
	}))
	if q, _ := s.Read(ctx).QueuedPrompts("a1"); len(q) != 0 {
		t.Errorf("a1's queue after both = %v", q)
	}
	// A claim whose typing never reached the terminal goes back in its place.
	must(s.Write(ctx, nil, "", func(tx *Tx) error { return tx.RequeuePrompt("p1") }))
	if q, _ := s.Read(ctx).QueuedPrompts("a1"); len(q) != 1 || q[0].Id != "p1" {
		t.Errorf("after the requeue = %v", q)
	}
	// Pruning forgets what left the queue long ago, never what is queued.
	s.now = func() time.Time { return fixed.Add(30 * 24 * time.Hour) }
	must(s.Write(ctx, nil, "", func(tx *Tx) error {
		n, err := tx.PrunePrompts(fixed.Add(24 * time.Hour))
		if err != nil || n != 1 {
			t.Errorf("pruned %d, %v", n, err)
		}
		return nil
	}))
	if q, _ := s.Read(ctx).QueuedPrompts("a1"); len(q) != 1 {
		t.Errorf("pruning took a queued prompt: %v", q)
	}
}
