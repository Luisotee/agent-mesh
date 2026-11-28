package store

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// seedRun stores a run with one of everything the store ties to a run or its agent.
func seedRun(t *testing.T, s *Store, run string) string {
	t.Helper()
	agent := "a-" + run
	ts := timestamppb.New(fixed)
	err := s.Write(context.Background(), nil, "", func(tx *Tx) error {
		steps := []error{
			tx.PutRun(&v1.Run{Id: run, Status: v1.RunStatus_RUN_STATUS_CLOSED, CreatedAt: ts}),
			tx.PutAgent(&v1.Agent{Id: agent, RunId: run, Name: "w", CreatedAt: ts}),
			tx.PutToken("hash-"+run, TokenRecord{Class: v1.ActorClass_ACTOR_CLASS_WORKER, SubjectID: agent, Name: "w", RunID: run}),
			tx.PutMessage(&v1.Message{Id: "m-" + run, RunId: run, ToId: agent, From: &v1.Actor{Id: "lead"}, Type: v1.MessageType_MESSAGE_TYPE_PLAN, CreatedAt: ts}),
			tx.PutAsk(&v1.Ask{Id: "k-" + run, RunId: run, CreatedAt: ts}),
			tx.PutHeldTask(&v1.HeldTask{Id: "h-" + run, RunId: run, CreatedAt: ts}),
			tx.QueuePrompt(&v1.QueuedPrompt{Id: "p-" + run, AgentId: agent, Text: "hi", CreatedAt: ts}),
			tx.PutAgentPR(&v1.PullRequest{AgentId: agent, Url: "https://github.com/o/r/pull/" + run}),
			tx.PutWorktree(&v1.Worktree{Id: "w-" + run, RunId: run, Slug: "s", CreatedAt: ts}),
		}
		for _, e := range steps {
			if e != nil {
				return e
			}
		}
		_, err := tx.AppendEvent(&v1.Event{RunId: run, Payload: &v1.Event_RunStopped{RunStopped: &v1.RunStopped{}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func TestDeleteRunRemovesEveryRowOfTheRunAndNoOtherRun(t *testing.T) {
	s := open(t)
	gone := seedRun(t, s, "r1")
	seedRun(t, s, "r2")
	var removed []string
	if err := s.Write(context.Background(), nil, "", func(tx *Tx) (err error) {
		removed, err = tx.DeleteRun("r1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != gone {
		t.Fatalf("removed agents = %v, want [%s]", removed, gone)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := s.r.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tc := range []struct{ run, agent string }{{"r1", "a-r1"}, {"r2", "a-r2"}} {
		want := 0
		if tc.run == "r2" {
			want = 1
		}
		for table, n := range map[string]int{
			"runs":       count(`SELECT COUNT(*) FROM runs WHERE id = ?`, tc.run),
			"agents":     count(`SELECT COUNT(*) FROM agents WHERE run_id = ?`, tc.run),
			"tokens":     count(`SELECT COUNT(*) FROM tokens WHERE subject_id = ?`, tc.agent),
			"messages":   count(`SELECT COUNT(*) FROM messages WHERE run_id = ?`, tc.run),
			"waiting":    count(`SELECT COUNT(*) FROM waiting WHERE to_id = ?`, tc.agent),
			"asks":       count(`SELECT COUNT(*) FROM asks WHERE run_id = ?`, tc.run),
			"held_tasks": count(`SELECT COUNT(*) FROM held_tasks WHERE run_id = ?`, tc.run),
			"prompts":    count(`SELECT COUNT(*) FROM prompts WHERE agent_id = ?`, tc.agent),
			"agent_prs":  count(`SELECT COUNT(*) FROM agent_prs WHERE agent_id = ?`, tc.agent),
			"worktrees":  count(`SELECT COUNT(*) FROM worktrees WHERE run_id = ?`, tc.run),
			"events":     count(`SELECT COUNT(*) FROM events WHERE run_id = ?`, tc.run),
		} {
			if n != want {
				t.Errorf("%s of %s: %d rows, want %d", table, tc.run, n, want)
			}
		}
	}
	// A run that is not there deletes nothing and is not an error: the caller tells a missing run first.
	if err := s.Write(context.Background(), nil, "", func(tx *Tx) error {
		ids, err := tx.DeleteRun("nope")
		if len(ids) != 0 {
			return fmt.Errorf("agents of a missing run: %v", ids)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
