package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Ask asks a question or has a pull request reviewed, by a fresh read-only agent or an existing one; with
// `reviewers` it is a round, collected (collect.go).
func (d *Daemon) Ask(ctx context.Context, req *connect.Request[v1.AskRequest]) (*connect.Response[v1.AskResponse], error) {
	actor := auth.ActorFrom(ctx)
	m := req.Msg
	if !IsAgent(actor.Class) {
		return nil, denied("an ask answers to an agent: start the agent with `mesh spawn`")
	}
	if mid := m.Meta.GetMutationId(); mid != "" {
		if proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid); err == nil && proc == agentmeshv1connect.MeshServiceAskProcedure {
			out := &v1.AskResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		}
	}
	body := strings.TrimSpace(m.Body)
	if strings.HasPrefix(m.Review, "-") {
		return nil, invalid("review %q looks like an option: give the PR's number or URL", m.Review)
	}
	if m.Since != "" && !commitRE.MatchString(m.Since) {
		return nil, invalid("since %q: a commit id (hex, 7 to 40 characters)", m.Since)
	}
	switch {
	case m.Peers != "":
		if len(m.Mcp) > 0 {
			return nil, invalid("mcp is for a fresh Claude asker or single reviewer, not peers")
		}
		return d.askPeers(ctx, actor, m, body)
	case m.Agent != "":
		if m.Repo != "" || m.Reviewers != "" || m.Slug != "" || m.Kind != "" || m.Tier != "" || m.Model != "" || m.Effort != "" || len(m.Mcp) > 0 {
			return nil, invalid("ask an existing agent with its name and a body (or --review): repo, reviewers, slug, kind, tier, model, effort and mcp are for a fresh one")
		}
		return d.askAgent(ctx, actor, m, body)
	case strings.TrimSpace(m.Repo) == "":
		return nil, invalid("name the agent to ask, or the repo (--repo) a fresh one works in")
	case m.Review == "":
		if m.Reviewers != "" || m.Since != "" || m.Size != "" {
			return nil, invalid("reviewers, size and since are for a review of a pull request (--review)")
		}
		if body == "" {
			return nil, invalid("no question given (--body)")
		}
		return d.askFresh(ctx, m, body)
	}
	if _, ok := sizes[m.Size]; !ok {
		return nil, invalid("size %q: trivial, small or large (it picks the reviewers, the /code-review level and the rounds)", m.Size)
	}
	if m.Reviewers != "" {
		return d.reviewRound(ctx, actor, m, body)
	}
	return d.reviewOne(ctx, actor, m, body)
}

// askTimeout is how long a collected ask waits: the request's seconds, else `ask.timeout`.
func (d *Daemon) askTimeout(r store.Reader, seconds int32) (time.Duration, error) {
	if seconds < 0 {
		return 0, invalid("timeout can't be negative")
	}
	if seconds > 0 {
		return time.Duration(seconds) * time.Second, nil
	}
	v, err := d.Setting(r, "ask.timeout")
	if err != nil {
		return 0, err
	}
	return time.Duration(v.(float64) * float64(time.Second)), nil
}

// subMeta is a mutation id for one step of an ask (a spawn, a task), derived from the ask's own, so a retry of
// the whole ask replays each step instead of starting a second reviewer.
func subMeta(m *v1.RequestMeta, step string) *v1.RequestMeta {
	if m.GetMutationId() == "" {
		return nil
	}
	return &v1.RequestMeta{MutationId: m.MutationId + "/" + step}
}

// askIDFor is the id of a round's ask: derived from the caller and the request's mutation id, so a retry of an
// interrupted round finds the ask it left (and its members) instead of opening a second one that would take
// their answers. Without a mutation id it is random.
func askIDFor(actor *v1.Actor, m *v1.RequestMeta) string {
	if m.GetMutationId() == "" {
		return ids.New("k", 10)
	}
	sum := sha256.Sum256([]byte(actor.Id + "\x00" + m.MutationId))
	return "k" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:10]
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// topOf is a repo's top level.
func topOf(ctx context.Context, repo string) (string, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", invalid("repo: %v", err)
	}
	top, err := git(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", invalid("%s is not a git repository: %v", repo, err)
	}
	return top, nil
}

// askFresh starts an asker with a read-only question. Its own `done` is the answer: no collecting.
func (d *Daemon) askFresh(ctx context.Context, m *v1.AskRequest, body string) (*connect.Response[v1.AskResponse], error) {
	actor := auth.ActorFrom(ctx)
	top, err := d.repoTop(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	meta := subMeta(m.Meta, "spawn")
	slug := m.Slug
	if slug == "" {
		slug = askSlug(body)
	}
	mcpNames, _, err := parseMCPArg(m.Mcp)
	if err != nil {
		return nil, err
	}
	// A retry of an ask whose spawn went through replays that spawn: no slug or guard to find again.
	if !d.spawnReplays(ctx, actor, meta) {
		d.warmOpenCodeModels(ctx, orClaude(m.Kind))
		if slug, err = d.freeSlug(d.store.Read(ctx), actor.RunId, top, slug); err != nil {
			return nil, err
		}
		if err := d.ReserveGuard(ctx, d.store.Read(ctx), actor.RunId, orClaude(m.Kind), m.Model, m.Effort, "an asker in "+top); err != nil {
			return nil, err
		}
	}
	res, err := d.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta, Repo: top, Slug: slug,
		Role: v1.AgentRole_AGENT_ROLE_ASKER, Kind: m.Kind, Model: m.Model, Effort: m.Effort, Tier: m.Tier,
		Task: askPrefix + "\n\n" + body, Mcp: mcpNames}))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.AskResponse{Agents: []*v1.Agent{res.Msg.Agent}}), nil
}

// spawnReplays tells whether a SpawnAgent with this meta is a retry of one that went through: it answers from
// the stored mutation, so the slug and the quota reserve are not looked at again (a quota that filled since is
// no reason to fail a retry whose agent already exists).
func (d *Daemon) spawnReplays(ctx context.Context, actor *v1.Actor, meta *v1.RequestMeta) bool {
	if meta == nil {
		return false
	}
	proc, _, err := d.store.Read(ctx).Mutation(actor.Id, meta.MutationId)
	return err == nil && proc == agentmeshv1connect.MeshServiceSpawnAgentProcedure
}

func orClaude(kind string) string {
	if kind == "" {
		return "claude"
	}
	return kind
}

// askAgent gives an existing agent of this run a read-only task: a question, or (with review) a review of a
// pull request from its own worktree. A retired agent is resumed, a busy one is told the task waits.
func (d *Daemon) askAgent(ctx context.Context, actor *v1.Actor, m *v1.AskRequest, body string) (*connect.Response[v1.AskResponse], error) {
	r := d.store.Read(ctx)
	a, err := d.FindAgent(r, actor, m.Agent)
	if err != nil {
		return nil, err
	}
	if a.ParentId != actor.Id {
		return nil, denied("%s is not yours to ask: an agent asks its own children", a.Name)
	}
	task := askPrefix + "\n\n" + body
	switch {
	case m.Review != "":
		if _, ok := sizes[m.Size]; !ok {
			return nil, invalid("size %q: trivial, small or large (it picks the /code-review level)", m.Size)
		}
		pr, err := d.resolvePR(ctx, a.Repo, m.Review)
		if err != nil {
			return nil, err
		}
		level, err := d.settingString(r, "review.level."+m.Size)
		if err != nil {
			return nil, err
		}
		spec := reviewSpec{pr: pr, kind: a.Kind, level: level, body: body, last: m.Since, ownBranch: true}
		spec.narrow = narrowSince(ctx, a.Repo, m.Since, pr.Head)
		spec.reused = m.Since != ""
		task = reviewTask(spec)
	case body == "":
		return nil, invalid("no question given (--body)")
	default:
		task = withOpenCodeBodyNote(a.Kind, task)
	}
	if err := d.ReserveGuard(ctx, d.store.Read(ctx), a.RunId, a.Kind, a.Model, a.Effort, "asking "+a.Name); err != nil {
		return nil, err
	}
	res, err := d.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: m.Meta, To: a.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: task}))
	if err != nil {
		return nil, err
	}
	out := &v1.AskResponse{Message: res.Msg.Message}
	if cur, err := d.getAgent(d.store.Read(ctx), a.Id); err == nil {
		out.Agents = []*v1.Agent{d.view(cur)}
		if cur.State == v1.AgentState_AGENT_STATE_WORKING {
			out.Notes = append(out.Notes, a.Name+" is working: your task waits in its inbox until it is idle")
		}
	}
	if res.Msg.Resumed != nil {
		out.Notes = append(out.Notes, a.Name+" was retired: resumed in its own conversation")
	}
	if res.Msg.ResumeError != "" {
		out.Notes = append(out.Notes, a.Name+" could not be resumed: "+res.Msg.ResumeError+" (the task is stored for when it is)")
	}
	return connect.NewResponse(out), nil
}

// reviewOne starts one fresh reviewer, at the PR's head. Its own `done` is the answer: no collecting, but the
// ask is kept, so a later round knows which head was reviewed.
func (d *Daemon) reviewOne(ctx context.Context, actor *v1.Actor, m *v1.AskRequest, body string) (*connect.Response[v1.AskResponse], error) {
	top, err := d.repoTop(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	pr, err := d.resolvePR(ctx, top, m.Review)
	if err != nil {
		return nil, err
	}
	r := d.store.Read(ctx)
	kind := orClaude(m.Kind)
	level, err := d.settingString(r, "review.level."+m.Size)
	if err != nil {
		return nil, err
	}
	effort := m.Effort
	if effort == "" && m.Model == "" {
		if effort, err = d.settingString(r, "review.effort"); err != nil {
			return nil, err
		}
	}
	spawnMeta := subMeta(m.Meta, "spawn")
	slug := fmt.Sprintf("review-%d-%s", pr.Number, kind)
	replay := d.spawnReplays(ctx, actor, spawnMeta)
	if !replay {
		if slug, err = d.freeSlug(r, actor.RunId, top, slug); err != nil {
			return nil, err
		}
	}
	last := m.Since
	task := reviewTask(reviewSpec{pr: pr, kind: kind, level: level, body: body, last: last,
		narrow: narrowSince(ctx, top, last, pr.Head)})
	mcpNames, _, err := parseMCPArg(m.Mcp)
	if err != nil {
		return nil, err
	}
	if !replay {
		d.warmOpenCodeModels(ctx, kind)
		if err := d.ReserveGuard(ctx, d.store.Read(ctx), actor.RunId, kind, m.Model, effort, "a reviewer in "+top); err != nil {
			return nil, err
		}
	}
	res, err := d.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: spawnMeta, Repo: top, Slug: slug,
		Role: v1.AgentRole_AGENT_ROLE_REVIEWER, Kind: m.Kind, Model: m.Model, Effort: effort, Tier: m.Tier, Base: pr.Head, Task: task, Mcp: mcpNames}))
	if err != nil {
		return nil, err
	}
	now := d.now()
	ask := &v1.Ask{Id: ids.New("k", 10), RunId: actor.RunId, CallerId: actor.Id, CallerName: actor.Name,
		Kind: v1.AskKind_ASK_KIND_REVIEW, Status: v1.AskStatus_ASK_STATUS_FINISHED, Pr: pr.URL, PrNumber: pr.Number, Repo: top,
		Head: pr.Head, Since: last, Size: m.Size, Tier: m.Tier, Body: body, CreatedAt: timestamppb.New(now), FinishedAt: timestamppb.New(now),
		Members: []*v1.AskMember{{AgentId: res.Msg.Agent.Id, Name: res.Msg.Agent.Name, Kind: res.Msg.Agent.Kind, How: "fresh",
			Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING, Line: "asked alone: its `done` comes to you directly",
			Model: res.Msg.Agent.Model, Tier: m.Tier, Head: pr.Head}}}
	out := &v1.AskResponse{Ask: ask, Agents: []*v1.Agent{res.Msg.Agent}}
	if _, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceAskProcedure, out, func(tx *store.Tx) error {
		return tx.PutAsk(ask)
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// reviewRound runs a round of reviewers at once (bin/mesh's review_round): it picks the kinds, starts a fresh
// reviewer of each at the PR's head (or, in the same run, asks the earlier round's own again), and records
// the ask the answers are collected by.
func (d *Daemon) reviewRound(ctx context.Context, actor *v1.Actor, m *v1.AskRequest, body string) (*connect.Response[v1.AskResponse], error) {
	if len(m.Mcp) > 0 {
		return nil, invalid("mcp is for a fresh Claude asker or single reviewer, not a round")
	}
	if m.Tier == "" {
		return nil, invalid("give the tier the reviewers run on (--tier): there is no default model")
	}
	if m.Model != "" || m.Kind != "" {
		return nil, invalid("a round picks its own kinds (--reviewers) on a tier: model and kind are for one reviewer")
	}
	top, err := d.repoTop(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	pr, err := d.resolvePR(ctx, top, m.Review)
	if err != nil {
		return nil, err
	}
	r := d.store.Read(ctx)
	prior, err := roundsOf(r, pr.URL)
	if err != nil {
		return nil, err
	}
	var last *v1.Ask
	if len(prior) > 0 {
		last = prior[len(prior)-1]
	}
	// A re-review: the head the last round covered, or the one named.
	since := m.Since
	if since == "" && last != nil && last.Head != pr.Head {
		since = last.Head
	}
	var earlierKinds []string
	if last != nil && last.Head != pr.Head {
		for _, mem := range last.Members {
			if mem.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
				earlierKinds = append(earlierKinds, mem.Kind)
			}
		}
	}
	d.warmOpenCodeModels(ctx, "opencode")
	kinds, notes, err := d.pickReviewers(r, m.Reviewers, m.Size, m.Tier, prAuthorKind(r, top, pr.HeadRef), earlierKinds, top)
	if err != nil {
		return nil, err
	}
	level, err := d.settingString(r, "review.level."+m.Size)
	if err != nil {
		return nil, err
	}
	effort := m.Effort
	if effort == "" {
		if effort, err = d.settingString(r, "review.effort"); err != nil {
			return nil, err
		}
	}
	timeout, err := d.askTimeout(r, m.Timeout)
	if err != nil {
		return nil, err
	}
	now := d.now()
	ask := &v1.Ask{Id: askIDFor(actor, m.Meta), RunId: actor.RunId, CallerId: actor.Id, CallerName: actor.Name,
		Kind: v1.AskKind_ASK_KIND_REVIEW, Status: v1.AskStatus_ASK_STATUS_OPEN, Pr: pr.URL, PrNumber: pr.Number, Repo: top,
		Head: pr.Head, Since: since, Size: m.Size, Tier: m.Tier, Body: body, Collected: true, Round: int32(len(prior) + 1),
		AnswersPath: d.answersPath(actor.RunId, pr.Number, now), CreatedAt: timestamppb.New(now),
		Deadline: timestamppb.New(now.Add(timeout))}
	out := &v1.AskResponse{Ask: ask, Notes: notes}
	narrow := narrowSince(ctx, top, since, pr.Head)
	// The same commit asked again is a new round of fresh reviewers; the earlier ones are reused only for a new head.
	reuseFrom := last
	if last != nil && last.Head == pr.Head {
		reuseFrom = nil
	}
	// Reviewers of the earlier round already running are part of this one whichever slot comes first: they share
	// its place in the cap from the start, so the order of the kinds never decides who the cap refuses.
	running := make([]string, len(kinds)) // by slot: "" for one with no running reviewer to reuse
	for i, k := range kinds {
		if a, _, _ := reusableReviewer(r, actor, reuseFrom, k); a != nil && hasProcess(a) {
			running[i] = a.Id
		}
	}
	return d.collectRound(ctx, actor, m.Meta, ask, out, kinds, true, running, func(ctx context.Context, i int) (*v1.AskMember, *v1.Agent, string) {
		return d.startReviewer(ctx, actor, reviewJob{kind: kinds[i], pr: pr, top: top, level: level, effort: effort,
			tier: m.Tier, body: body, since: since, narrow: narrow, last: reuseFrom, meta: m.Meta})
	}, func(r store.Reader, a *v1.Ask) string { return d.roundsHint(r, a, false) })
}

// collectRound starts the members of a collected ask one after another and returns once they have started
// (the answers come later, collect.go). The ask is stored before its first member starts and can't finish until
// the last has (starting), so an answer that comes while the others are still starting is collected. keys name
// the members (a reviewer's kind, a peer's name) and start(ctx, i) brings member i in: a member that can't be
// reached is one with a line saying why, not an error. With oneSlot the members of the round share one place in
// `caps.agents` (a review round: its first reviewer must fit the cap, the others then join it, so the round is
// never cut short by it, and the agents of `running` (by key: one already running for that slot, "" for none) already hold it, until their slot fails); without, each counts as the agent it is (a
// peers group can be many). A mutation id of the request covers the whole ask: its id comes from it, so a retry after an
// interruption finds the ask it left, keeps the members already started (their own spawns replay too) and starts the rest; the final write records the mutation.
func (d *Daemon) collectRound(ctx context.Context, actor *v1.Actor, meta *v1.RequestMeta, ask *v1.Ask, out *v1.AskResponse, keys []string, oneSlot bool, running []string,
	start func(ctx context.Context, i int) (*v1.AskMember, *v1.Agent, string), hint func(store.Reader, *v1.Ask) string) (*connect.Response[v1.AskResponse], error) {
	have := map[string]*v1.AskMember{}
	if prev, err := d.store.Read(ctx).GetAsk(ask.Id); err == nil {
		if prev.Status != v1.AskStatus_ASK_STATUS_OPEN || !prev.Starting {
			return nil, d.noReviewer(prev)
		}
		ask = prev
		out.Ask = prev
		for _, mem := range prev.Members {
			if prev.Kind == v1.AskKind_ASK_KIND_PEERS {
				have[mem.Name] = mem
			} else {
				have[mem.Kind] = mem
			}
		}
	} else if errors.Is(err, store.ErrNotFound) {
		ask.Starting = true
		if err := d.store.Write(ctx, actor, "", func(tx *store.Tx) error { return tx.PutAsk(ask) }); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	started := 0
	var shared []string // the agents of this round already running, whose place in the cap the others share
	for _, id := range running {
		if id != "" {
			shared = append(shared, id)
		}
	}
	for i, key := range keys {
		if mem := have[key]; mem != nil {
			if mem.AgentId != "" && mem.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
				if !slices.Contains(shared, mem.AgentId) {
					shared = append(shared, mem.AgentId)
				}
				if cur, err := d.getAgent(d.store.Read(ctx), mem.AgentId); err == nil {
					out.Agents = append(out.Agents, d.view(cur))
				}
				started++
			}
			continue
		}
		startCtx := ctx
		if oneSlot {
			startCtx = withCapShared(ctx, shared)
		}
		member, agent, note := start(startCtx, i)
		if note != "" {
			out.Notes = append(out.Notes, note)
		}
		if agent != nil {
			out.Agents = append(out.Agents, agent)
			if !slices.Contains(shared, agent.Id) {
				shared = append(shared, agent.Id)
			}
			started++
		} else if i < len(running) && running[i] != "" {
			// Its reviewer was not asked after all: it is no part of the round, and counts against the cap again.
			shared = slices.DeleteFunc(shared, func(id string) bool { return id == running[i] })
		}
		if err := d.store.Write(ctx, actor, "", func(tx *store.Tx) error {
			cur, err := tx.GetAsk(ask.Id)
			if err != nil {
				return err
			}
			cur.Members = append(cur.Members, member)
			return tx.PutAsk(cur)
		}); err != nil {
			return nil, err
		}
	}
	if started == 0 {
		cur, err := d.store.Read(ctx).GetAsk(ask.Id)
		if err != nil {
			return nil, err
		}
		cur.Starting, cur.Status, cur.FinishedAt = false, v1.AskStatus_ASK_STATUS_FINISHED, timestamppb.New(d.now())
		if err := d.store.Write(ctx, actor, "", func(tx *store.Tx) error { return tx.PutAsk(cur) }); err != nil {
			return nil, err
		}
		return nil, d.noReviewer(cur)
	}
	var ring *v1.Message
	if _, err := d.write(ctx, meta, agentmeshv1connect.MeshServiceAskProcedure, out, func(tx *store.Tx) error {
		cur, err := tx.GetAsk(ask.Id)
		if err != nil {
			return err
		}
		cur.Starting = false
		out.Ask = cur
		// Members that answered or settled while the others started finish it now.
		ring, err = d.finishIfDone(tx, cur)
		out.Hint = hint(tx.Reader, cur)
		return err
	}); err != nil {
		return nil, err
	}
	if ring != nil && d.notify != nil {
		d.notify(ring.ToId)
	}
	return connect.NewResponse(out), nil
}

// noReviewer is the refusal of a round none of whose members could be started.
func (d *Daemon) noReviewer(a *v1.Ask) error {
	var why []string
	for _, mem := range a.Members {
		why = append(why, mem.Name+": "+mem.Line)
	}
	what := "no reviewer could be started"
	if a.Kind == v1.AskKind_ASK_KIND_PEERS {
		what = "nobody could be asked"
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s (%s)", what, strings.Join(why, "; ")))
}

// reviewJob is what starting or re-asking one reviewer of a round needs.
type reviewJob struct {
	kind, top, level, effort, tier, body, since string
	pr                                          *prInfo
	narrow                                      bool
	last                                        *v1.Ask
	meta                                        *v1.RequestMeta
}

// reusableReviewer is the earlier round's own reviewer of a kind that a re-review asks again: the agent, the head
// it reviewed, and whether a reviewer of the kind was in that round without answering it (it has not seen the
// earlier commits, so a fresh one reviews the whole PR).
func reusableReviewer(r store.Reader, actor *v1.Actor, last *v1.Ask, kind string) (reuse *v1.Agent, reviewedAt string, unanswered bool) {
	if last == nil || last.RunId != actor.RunId {
		return nil, "", false
	}
	for _, mem := range last.Members {
		if mem.Kind != kind || mem.AgentId == "" || mem.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
			continue
		}
		if mem.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED {
			unanswered = true
			continue
		}
		if a, err := r.GetAgent(mem.AgentId); err == nil && a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER && a.ParentId == actor.Id {
			return a, mem.Head, false
		}
	}
	return nil, "", unanswered
}

// startReviewer brings one reviewer of a round into it: the earlier round's own agent when this run has one
// (given a new task, resumed if retired, its worktree moved to the new head when that is safe), else a fresh
// agent at the head. A reviewer that can't be reached is a member with a line saying why, not an error.
func (d *Daemon) startReviewer(ctx context.Context, actor *v1.Actor, j reviewJob) (*v1.AskMember, *v1.Agent, string) {
	member := &v1.AskMember{Kind: j.kind, Tier: j.tier, Head: j.pr.Head, Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING}
	fail := func(name, line string) (*v1.AskMember, *v1.Agent, string) {
		member.Name = name
		member.Status, member.Line = v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED, line
		return member, nil, fmt.Sprintf("%s (%s): not asked: %s", name, j.kind, line)
	}
	r := d.store.Read(ctx)
	reuse, reviewedAt, unanswered := reusableReviewer(r, actor, j.last, j.kind)
	// An OpenCode reviewer that ran on a zero-cost model before the repo was set against them (freemodels.go) is not
	// sent the new head: a fresh one starts on a model the tier allows, or is refused.
	var freshNote string
	var notReused *v1.Agent // retired once its replacement has started
	if reuse != nil && d.freeOffFor(r, reuse.Kind, reuse.Repo) != "" {
		d.warmOpenCodeModels(ctx, opencode.Kind)
		at, _, _ := target(reuse) // where it will run, when a reopen is pending
		if err := d.checkKept(r, reuse, at); err != nil {
			freshNote = fmt.Sprintf("%s is on a model its repo keeps code from (repos.no_free_models): a new %s reviewer instead", reuse.Name, j.kind)
			notReused, reuse, unanswered = reuse, nil, true
		}
	}
	if reuse != nil {
		member.Name = reuse.Name
		model, effort, _ := target(reuse) // where it will run, when a reopen is pending
		if err := d.ReserveGuard(ctx, d.store.Read(ctx), actor.RunId, reuse.Kind, model, effort, "asking "+reuse.Name); err != nil {
			return fail(reuse.Name, err.Error())
		}
		spec := reviewSpec{pr: j.pr, kind: j.kind, level: j.level, body: j.body, last: reviewedAt, reused: true,
			narrow: narrowSince(ctx, j.top, reviewedAt, j.pr.Head)}
		var note string
		// The move and the read of where it stayed are one step under the worktree's lock; SendMessage takes the
		// lock itself, so it is let go before.
		mu := d.worktreeLock(reuse.Worktree)
		mu.Lock()
		if why := moveWorktree(ctx, reuse, reviewedAt, j.pr.Head); why != "" {
			spec.stayAt, spec.stayWhy = reviewedAt, why
			if cur, err := git(ctx, reuse.Worktree, "rev-parse", "HEAD"); err == nil {
				spec.stayAt = cur
			}
			note = fmt.Sprintf("%s's worktree stays at %s: %s", reuse.Name, short(spec.stayAt), why)
		}
		mu.Unlock()
		res, err := d.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: subMeta(j.meta, "send-"+j.kind), To: reuse.Id,
			Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: reviewTask(spec)}))
		if err != nil {
			return fail(reuse.Name, err.Error())
		}
		if res.Msg.ResumeError != "" {
			// The task was stored with the refusal: not for the next resume, so it is marked read.
			d.dropStoredTask(ctx, actor, reuse.Id, res.Msg.Message.GetId())
			return fail(reuse.Name, "could not be resumed: "+res.Msg.ResumeError)
		}
		member.AgentId = reuse.Id // only a reviewer that took the task is the member's agent
		member.How, member.Model = "asked", reuse.Model
		member.Tier = reuse.Tier
		if reuse.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING && reuse.ExecTier != "" {
			member.Tier = reuse.ExecTier
		}
		if res.Msg.Resumed != nil {
			member.How = "resumed"
		}
		cur, _ := d.getAgent(d.store.Read(ctx), reuse.Id)
		if cur == nil {
			cur = reuse
		}
		return member, d.view(cur), note
	}
	// A fresh one.
	// A retry whose spawn went through replays it: the slug and the reserve are not looked at again.
	spawnMeta := subMeta(j.meta, "spawn-"+j.kind)
	slug := fmt.Sprintf("review-%d-%s", j.pr.Number, j.kind)
	if !d.spawnReplays(ctx, actor, spawnMeta) {
		var err error
		if slug, err = d.freeSlug(r, actor.RunId, j.top, slug); err != nil {
			return fail(j.kind, err.Error())
		}
		member.Name = filepath.Base(j.top) + "-" + slug
		if err := d.ReserveGuard(ctx, d.store.Read(ctx), actor.RunId, j.kind, "", j.effort, "a "+j.kind+" reviewer in "+j.top); err != nil {
			return fail(member.Name, err.Error())
		}
	} else {
		member.Name = filepath.Base(j.top) + "-" + slug
	}
	spec := reviewSpec{pr: j.pr, kind: j.kind, level: j.level, body: j.body, last: j.since, narrow: j.narrow}
	if unanswered {
		spec.last, spec.narrow = "", false
	}
	task := reviewTask(spec)
	res, err := d.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: spawnMeta, Repo: j.top, Slug: slug,
		Role: v1.AgentRole_AGENT_ROLE_REVIEWER, Kind: j.kind, Effort: j.effort, Tier: j.tier, Base: j.pr.Head, Task: task}))
	if err != nil {
		var ce *connect.Error
		msg := err.Error()
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		if freshNote != "" {
			msg = freshNote + "; " + msg // why a new one was wanted, with why it could not start
		}
		return fail(member.Name, msg)
	}
	member.AgentId, member.Name, member.How, member.Model = res.Msg.Agent.Id, res.Msg.Agent.Name, "fresh", res.Msg.Agent.Model
	if notReused != nil {
		// The earlier reviewer, on a model its repo no longer allows, is not left running beside its replacement.
		if _, err := d.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: subMeta(j.meta, "retire-"+j.kind), Agent: notReused.Id})); err != nil {
			d.log.Warn("retiring a reviewer that was not reused", "agent", notReused.Name, "err", err)
		}
	}
	return member, res.Msg.Agent, freshNote
}

// ── verdicts and listing ──

// asksOf finds the asks about a PR given by URL or number, in the caller's run (a device: any).
func asksOf(r store.Reader, actor *v1.Actor, pr string) ([]*v1.Ask, error) {
	var asks []*v1.Ask
	var err error
	if n, convErr := strconv.Atoi(strings.TrimPrefix(pr, "#")); convErr == nil {
		asks, err = r.NumberAsks(int32(n))
	} else {
		// A URL as the host stored it, else as a person types it (bin/mesh's pr_key): any case, a trailing slash,
		// a query or a fragment, found by the number at its end.
		asks, err = r.PRAsks(pr)
		if err == nil && len(asks) == 0 {
			key := prKey(pr)
			if n, convErr := strconv.Atoi(key[strings.LastIndex(key, "/")+1:]); convErr == nil {
				var all []*v1.Ask
				if all, err = r.NumberAsks(int32(n)); err == nil {
					for _, a := range all {
						if prKey(a.Pr) == key {
							asks = append(asks, a)
						}
					}
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	own := runScope(actor)
	var out []*v1.Ask
	prs := map[string]bool{}
	for _, a := range asks {
		if own == "" || a.RunId == own {
			out = append(out, a)
			prs[a.Pr] = true
		}
	}
	if len(prs) > 1 {
		var urls []string
		for u := range prs {
			urls = append(urls, u)
		}
		sort.Strings(urls)
		return nil, invalid("%s is the number of several pull requests (%s): give the URL", pr, strings.Join(urls, ", "))
	}
	return out, nil
}

// prKey is a PR URL as one key: lower case, with no query, fragment or trailing slash.
func prKey(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(strings.TrimRight(u, "/"))
}

// RecordVerdict stores the root's judgement of one reviewer's findings on the PR's newest round.
func (d *Daemon) RecordVerdict(ctx context.Context, req *connect.Request[v1.RecordVerdictRequest]) (*connect.Response[v1.RecordVerdictResponse], error) {
	m := req.Msg
	if m.Pr == "" || m.Reviewer == "" {
		return nil, invalid("name the pull request and the reviewer")
	}
	if m.Agree < 0 || m.Other < 0 || m.Disagree < 0 || m.Blocking < 0 {
		return nil, invalid("counts can't be negative")
	}
	if m.Agree == 0 && m.Other == 0 && m.Disagree == 0 {
		return nil, invalid("give at least one of agree, other and disagree: how many of the reviewer's findings you agreed with, found otherwise useful, and disagreed with")
	}
	if m.Blocking > m.Agree+m.Other {
		return nil, invalid("blocking (%d) can't be more than agree + other (%d): a blocking finding is one you agreed with or found useful", m.Blocking, m.Agree+m.Other)
	}
	out := &v1.RecordVerdictResponse{}
	_, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceRecordVerdictProcedure, out, func(tx *store.Tx) error {
		asks, err := asksOf(tx.Reader, tx.Actor, m.Pr)
		if err != nil {
			return err
		}
		for i := len(asks) - 1; i >= 0; i-- {
			ask := asks[i]
			if ask.Kind != v1.AskKind_ASK_KIND_REVIEW {
				continue
			}
			for _, mem := range ask.Members {
				if mem.Name != m.Reviewer && mem.AgentId != m.Reviewer {
					continue
				}
				tier := mem.Tier
				if a, err := tx.GetAgent(mem.AgentId); err == nil && a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING && a.ExecTier != "" {
					tier = a.ExecTier
				}
				v := &v1.Verdict{Reviewer: mem.Name, Kind: mem.Kind, Model: mem.Model, Tier: tier, Agree: m.Agree, Other: m.Other,
					Disagree: m.Disagree, Blocking: m.Blocking, At: timestamppb.New(tx.Now())}
				kept := ask.Verdicts[:0]
				for _, old := range ask.Verdicts {
					if old.Reviewer != mem.Name {
						kept = append(kept, old)
					}
				}
				ask.Verdicts = append(kept, v)
				out.Ask, out.Verdict = ask, v
				return tx.PutAsk(ask)
			}
		}
		return notFound("no review of %s asked %s", m.Pr, m.Reviewer)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// ListAsks lists a run's asks.
func (d *Daemon) ListAsks(ctx context.Context, req *connect.Request[v1.ListAsksRequest]) (*connect.Response[v1.ListAsksResponse], error) {
	actor := auth.ActorFrom(ctx)
	runID, err := scopedRun(actor, req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	r := d.store.Read(ctx)
	var asks []*v1.Ask
	if req.Msg.Pr != "" {
		if asks, err = asksOf(r, actor, req.Msg.Pr); err != nil {
			return nil, err
		}
	} else if asks, err = r.RunAsks(runID); err != nil {
		return nil, err
	}
	out := &v1.ListAsksResponse{}
	for _, a := range asks {
		if runID != "" && a.RunId != runID || req.Msg.OpenOnly && a.Status != v1.AskStatus_ASK_STATUS_OPEN {
			continue
		}
		out.Asks = append(out.Asks, a)
	}
	return connect.NewResponse(out), nil
}
