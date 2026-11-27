package daemon_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

func val(t *testing.T, v any) *structpb.Value {
	t.Helper()
	out, err := structpb.NewValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func setting(t *testing.T, h *harness, key string) *v1.Setting {
	t.Helper()
	res, err := h.device.Mesh.ListSettings(context.Background(), connect.NewRequest(&v1.ListSettingsRequest{Prefix: key}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Msg.Settings {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no setting %s in %v", key, res.Msg.Settings)
	return nil
}

func settingEvents(t *testing.T, h *harness) []*v1.Event {
	t.Helper()
	evs, err := h.d.Store().Read(context.Background()).EventsAfter(0, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.Event
	for _, e := range evs {
		if e.GetSettingChanged() != nil {
			out = append(out, e)
		}
	}
	return out
}

func TestSettingsListWithDefaultsAndSchemas(t *testing.T) {
	h := start(t, "")
	res, err := h.device.Mesh.ListSettings(context.Background(), connect.NewRequest(&v1.ListSettingsRequest{Prefix: "tiers.claude.mid."}))
	if err != nil || len(res.Msg.Settings) != 3 {
		t.Fatalf("%v, %v", res, err)
	}
	m := setting(t, h, "tiers.claude.mid.model")
	if m.IsSet || m.UpdatedBy != nil || m.UpdatedAt != nil || m.Schema.Type != "chain" || m.Schema.Summary == "" || m.Schema.Label != "Claude, mid tier: models" || m.Schema.Group != "Agents and models" || m.Schema.Advanced ||
		m.Value.GetListValue().GetValues()[0].GetStringValue() != "sonnet" || m.Default.GetListValue() == nil {
		t.Errorf("%v", m)
	}
	e := setting(t, h, "tiers.claude.mid.effort")
	if e.Schema.Type != "effort" || len(e.Schema.Choices) != 5 || e.Value.GetStringValue() != "high" {
		t.Errorf("%v", e)
	}
	if all, _ := h.device.Mesh.ListSettings(context.Background(), connect.NewRequest(&v1.ListSettingsRequest{})); len(all.Msg.Settings) < 30 {
		t.Errorf("%d settings in all", len(all.Msg.Settings))
	}
}

func TestSettingAValueRecordsWhoAndEmitsAGlobalEvent(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	set := func(mid string, v any) (*v1.Setting, error) {
		res, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(mid), Key: "quota_stop", Value: val(t, v)}))
		if err != nil {
			return nil, err
		}
		return res.Msg.Setting, nil
	}
	s, err := set("s-1", 75.0)
	if err != nil || !s.IsSet || s.Value.GetNumberValue() != 75 || s.Default.GetNumberValue() != 90 ||
		s.UpdatedBy.GetClass() != v1.ActorClass_ACTOR_CLASS_DEVICE || s.UpdatedAt == nil {
		t.Fatalf("%v, %v", s, err)
	}
	if got := setting(t, h, "quota_stop"); !got.IsSet || got.Value.GetNumberValue() != 75 || got.UpdatedBy.GetId() != s.UpdatedBy.GetId() {
		t.Errorf("listed %v", got)
	}
	// A retry with the same mutation id is the first result, and no second event.
	if again, err := set("s-1", 75.0); err != nil || again.Value.GetNumberValue() != 75 {
		t.Fatalf("%v, %v", again, err)
	}
	if evs := settingEvents(t, h); len(evs) != 1 {
		t.Fatalf("%d events after a retry", len(evs))
	}
	if _, err := set("s-2", 60.0); err != nil {
		t.Fatal(err)
	}
	evs := settingEvents(t, h)
	if len(evs) != 2 {
		t.Fatalf("%d events", len(evs))
	}
	first, second := evs[0], evs[1]
	if c := first.GetSettingChanged(); c.Key != "quota_stop" || c.Value.GetNumberValue() != 75 || c.Previous.GetNumberValue() != 90 || c.ToDefault {
		t.Errorf("first %v", c)
	}
	if first.RunId != "" || first.AgentId != "" || first.MutationId != "s-1" || first.Actor.GetClass() != v1.ActorClass_ACTOR_CLASS_DEVICE {
		t.Errorf("first event's frame %v", first)
	}
	if c := second.GetSettingChanged(); c.Value.GetNumberValue() != 60 || c.Previous.GetNumberValue() != 75 {
		t.Errorf("second %v", c)
	}

	res, err := h.device.Mesh.ResetSetting(ctx, connect.NewRequest(&v1.ResetSettingRequest{Meta: meta("s-3"), Key: "quota_stop"}))
	if err != nil || res.Msg.Setting.IsSet || res.Msg.Setting.Value.GetNumberValue() != 90 || res.Msg.Setting.UpdatedBy != nil {
		t.Fatalf("%v, %v", res, err)
	}
	evs = settingEvents(t, h)
	if c := evs[2].GetSettingChanged(); !c.ToDefault || c.Value.GetNumberValue() != 90 || c.Previous.GetNumberValue() != 60 {
		t.Errorf("reset event %v", c)
	}
	if got := setting(t, h, "quota_stop"); got.IsSet {
		t.Errorf("still set after a reset: %v", got)
	}
}

func TestASettingValueMustFitItsSchema(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	for _, c := range []struct {
		key   string
		value any
		want  string
	}{
		{"quota_stop", 101.0, "quota_stop must be a whole number 0 to 100"},
		{"quota_stop", "ninety", "a whole number 0 to 100"},
		{"tiers.claude.mid.effort", "ultra", "one of low, medium, high, xhigh, max"},
		{"tiers.claude.mid.model", []any{}, "a non-empty list of model names"},
		{"review.reviewers.small", []any{[]any{"gpt"}}, "a list of slots"},
		{"min_free_mb", "lots", `"auto" or a whole number of MB`},
		{"skip_permissions", "yes", "true or false"},
		{"idle_retire", -5.0, "a finite number of seconds"},
		{"caps.agents", 2.5, "a whole number"},
		{"quota_stop", nil, "a whole number 0 to 100"},
	} {
		_, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: c.key, Value: val(t, c.value)}))
		if code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s = %v: %v, want InvalidArgument naming %q", c.key, c.value, err, c.want)
		}
	}
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "quota_stop"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("no value: %v", err)
	}
	for _, key := range []string{"nope", "tiers.claude.mid", "quota"} {
		if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: key, Value: val(t, 1.0)})); code(err) != connect.CodeNotFound {
			t.Errorf("set %q: %v", key, err)
		}
		if _, err := h.device.Mesh.ResetSetting(ctx, connect.NewRequest(&v1.ResetSettingRequest{Meta: meta(""), Key: key})); code(err) != connect.CodeNotFound {
			t.Errorf("reset %q: %v", key, err)
		}
	}
	if evs := settingEvents(t, h); len(evs) != 0 {
		t.Errorf("a refused write left %d events", len(evs))
	}
	// A whole number written as 8.0 reads back as one, and a chain keeps its order.
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "tiers.agy.strong.model",
		Value: val(t, []any{"hydrogen", "neon"})})); err != nil {
		t.Fatal(err)
	}
	got := setting(t, h, "tiers.agy.strong.model").Value.GetListValue().GetValues()
	if len(got) != 2 || got[0].GetStringValue() != "hydrogen" || got[1].GetStringValue() != "neon" {
		t.Errorf("chain %v", got)
	}
}

func TestOnlyADeviceOrARootWritesSettingsAndEveryoneReadsThem(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	root := h.seedAgent(run.Id, "r1", "repo-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	worker := h.seedAgent(run.Id, "w1", "repo-fix", "r1", v1.AgentRole_AGENT_ROLE_WORKER)
	asker := h.seedAgent(run.Id, "a1", "repo-ask", "r1", v1.AgentRole_AGENT_ROLE_ASKER)
	reviewer := h.seedAgent(run.Id, "v1", "repo-rev", "r1", v1.AgentRole_AGENT_ROLE_REVIEWER)
	for name, tok := range map[string]string{"worker": worker, "asker": asker, "reviewer": reviewer, "root": root} {
		c := h.as(tok)
		if res, err := c.ListSettings(ctx, connect.NewRequest(&v1.ListSettingsRequest{Prefix: "caps."})); err != nil || len(res.Msg.Settings) != 2 {
			t.Errorf("%s lists: %v, %v", name, res, err)
		}
		_, err := c.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "caps.agents", Value: val(t, 4.0)}))
		_, rerr := c.ResetSetting(ctx, connect.NewRequest(&v1.ResetSettingRequest{Meta: meta(""), Key: "caps.agents"}))
		if name == "root" {
			if err != nil || rerr != nil {
				t.Errorf("a root sets and resets: %v, %v", err, rerr)
			}
			continue
		}
		if code(err) != connect.CodePermissionDenied || code(rerr) != connect.CodePermissionDenied {
			t.Errorf("%s: set %v, reset %v", name, err, rerr)
		}
	}
	evs := settingEvents(t, h)
	if len(evs) != 2 || evs[0].Actor.GetId() != "r1" || evs[0].Actor.GetClass() != v1.ActorClass_ACTOR_CLASS_ROOT {
		t.Errorf("the root's writes: %v", evs)
	}
}

func TestASettingChangeReachesARunsSubscribersAndSurvivesARestart(t *testing.T) {
	state := shortdir.State(t)
	h := start(t, state)
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	worker := h.seedAgent(run.Id, "w1", "repo-fix", "", v1.AgentRole_AGENT_ROLE_WORKER)
	s, err := h.as(worker).SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap := recv(t, s).GetSnapshot()
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "idle_retire", Value: val(t, 60.0)})); err != nil {
		t.Fatal(err)
	}
	ev := recv(t, s).GetEvent()
	if c := ev.GetSettingChanged(); c == nil || c.Key != "idle_retire" || ev.RunId != "" {
		t.Fatalf("the worker's stream got %v", ev)
	}
	// Replayed from a cursor, a run's subscriber gets the global event too.
	r, err := h.as(worker).SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Cursor: snap.Cursor}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recv(t, r) // Resumed
	if c := recv(t, r).GetEvent().GetSettingChanged(); c == nil || c.Key != "idle_retire" {
		t.Errorf("replay %v", c)
	}

	h.stop()
	h2 := start(t, state)
	got := setting(t, h2, "idle_retire")
	if !got.IsSet || got.Value.GetNumberValue() != 60 {
		t.Errorf("after a restart %v", got)
	}
	if v, err := h2.d.Setting(h2.d.Store().Read(ctx), "idle_retire"); err != nil || v != 60.0 {
		t.Errorf("the daemon reads %v, %v", v, err)
	}
	if v, err := h2.d.Setting(h2.d.Store().Read(ctx), "caps.agents"); err != nil || v != "auto" {
		t.Errorf("an unset setting reads %v, %v, want its default", v, err)
	}
	// A stored value the leaf's rule no longer takes falls back to the default, and the API still shows it.
	err = h2.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutSetting("tiers.claude.mid.effort", `"ultra"`) })
	if err != nil {
		t.Fatal(err)
	}
	if v, err := h2.d.Setting(h2.d.Store().Read(ctx), "tiers.claude.mid.effort"); err != nil || v != "high" {
		t.Errorf("an invalid stored effort reads %v, %v, want the default", v, err)
	}
}

func TestASettingThatDidNotChangeEmitsNoEvent(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	reset := func() {
		if _, err := h.device.Mesh.ResetSetting(ctx, connect.NewRequest(&v1.ResetSettingRequest{Meta: meta(""), Key: "caps.agents"})); err != nil {
			t.Fatal(err)
		}
	}
	set := func(n float64) {
		if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "caps.agents", Value: val(t, n)})); err != nil {
			t.Fatal(err)
		}
	}
	reset() // never set
	set(4)
	set(4) // the same value again, under another mutation id
	if evs := settingEvents(t, h); len(evs) != 1 {
		t.Fatalf("%d events, want 1", len(evs))
	}
	reset()
	reset()
	if evs := settingEvents(t, h); len(evs) != 2 || !evs[1].GetSettingChanged().ToDefault {
		t.Fatalf("events %v", evs)
	}
	// Setting the default value explicitly is a change: it is now an override.
	set(8)
	if evs := settingEvents(t, h); len(evs) != 3 {
		t.Fatalf("%d events after setting the default explicitly", len(evs))
	}
}
