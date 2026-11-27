package daemon

import (
	"context"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Setting is a setting's effective value: the stored override, else the leaf's default. The daemon's own reads
// (the caps, the retire delay, the guards) go through it, so they follow a change at once. A stored value that
// no longer fits the leaf's type (a rule tightened since) is logged and the default used; a failed read is an
// error, never a quiet default. An unknown key is a programming error and panics.
func (d *Daemon) Setting(r store.Reader, key string) (any, error) {
	l, ok := d.schema.Lookup(key)
	if !ok {
		panic("daemon: unknown setting " + key)
	}
	rec, err := r.Setting(key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return l.Default, nil
	case err != nil:
		return nil, err
	}
	var v any
	if err := json.Unmarshal([]byte(rec.Value), &v); err == nil {
		if v, err = settings.Check(l, v); err == nil {
			return v, nil
		}
	}
	d.log.Warn("stored setting is invalid; using its default", "key", key, "value", rec.Value)
	return l.Default, nil
}

// settingOf is the API's view of a leaf, with its override (rec, when the setting was set).
func settingOf(l settings.Leaf, rec *store.SettingRecord) (*v1.Setting, error) {
	def, err := structpb.NewValue(l.Default)
	if err != nil {
		return nil, err
	}
	out := &v1.Setting{Key: l.Key, Value: def, Default: def, Schema: &v1.SettingSchema{
		Type: string(l.Type), Summary: l.Summary, Choices: l.Choices, Restart: l.Restart}}
	if rec == nil {
		return out, nil
	}
	var v any
	if err := json.Unmarshal([]byte(rec.Value), &v); err != nil {
		return nil, err
	}
	if out.Value, err = structpb.NewValue(v); err != nil {
		return nil, err
	}
	out.IsSet = true
	out.UpdatedAt = tsOf(rec.UpdatedAt)
	out.UpdatedBy = rec.UpdatedBy
	return out, nil
}

func (d *Daemon) readSetting(r store.Reader, l settings.Leaf) (*v1.Setting, error) {
	rec, err := r.Setting(l.Key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return settingOf(l, nil)
	case err != nil:
		return nil, err
	}
	return settingOf(l, &rec)
}

// leaf finds a setting's definition; a key no setting has is NotFound.
func (d *Daemon) leaf(key string) (settings.Leaf, error) {
	l, ok := d.schema.Lookup(key)
	if !ok {
		return l, notFound("no setting %q (mesh settings lists them)", key)
	}
	return l, nil
}

// ListSettings lists the settings under a prefix.
func (d *Daemon) ListSettings(ctx context.Context, req *connect.Request[v1.ListSettingsRequest]) (*connect.Response[v1.ListSettingsResponse], error) {
	out := &v1.ListSettingsResponse{}
	err := d.store.Snapshot(ctx, func(r store.Reader) error {
		recs, err := r.Settings()
		if err != nil {
			return err
		}
		set := map[string]*store.SettingRecord{}
		for i := range recs {
			set[recs[i].Key] = &recs[i]
		}
		for _, key := range d.schema.Keys(req.Msg.Prefix) {
			l, _ := d.schema.Lookup(key)
			s, err := settingOf(l, set[key])
			if err != nil {
				return err
			}
			out.Settings = append(out.Settings, s)
		}
		return nil
	})
	return connect.NewResponse(out), err
}

// changeSetting stores or drops a setting's override and records the event. value nil means reset.
func (d *Daemon) changeSetting(t *store.Tx, l settings.Leaf, value any, reset bool) (*v1.Setting, error) {
	before, err := d.readSetting(t.Reader, l)
	if err != nil {
		return nil, err
	}
	if reset {
		err = t.DeleteSetting(l.Key)
	} else {
		var b []byte
		if b, err = json.Marshal(value); err == nil {
			err = t.PutSetting(l.Key, string(b))
		}
	}
	if err != nil {
		return nil, err
	}
	after, err := d.readSetting(t.Reader, l)
	if err != nil {
		return nil, err
	}
	if before.IsSet == after.IsSet && proto.Equal(before.Value, after.Value) {
		return after, nil // nothing changed (the same value again, or a reset of a setting nobody set): no event
	}
	// Global: no run, so every subscriber gets it.
	if _, err := t.AppendEvent(&v1.Event{Payload: &v1.Event_SettingChanged{SettingChanged: &v1.SettingChanged{
		Key: l.Key, Value: after.Value, Previous: before.Value, ToDefault: reset}}}); err != nil {
		return nil, err
	}
	return after, nil
}

// SetSetting sets a setting to a value that fits its schema.
func (d *Daemon) SetSetting(ctx context.Context, req *connect.Request[v1.SetSettingRequest]) (*connect.Response[v1.SetSettingResponse], error) {
	l, err := d.leaf(req.Msg.Key)
	if err != nil {
		return nil, err
	}
	if err := hostOnly(ctx, l.Key); err != nil {
		return nil, err
	}
	if req.Msg.Value == nil {
		return nil, invalid("%s: no value given (a JSON null is a value; use unset to reset)", l.Key)
	}
	v, err := settings.Check(l, req.Msg.Value.AsInterface())
	if err != nil {
		return nil, invalid("%v", err)
	}
	out := &v1.SetSettingResponse{}
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceSetSettingProcedure, out, func(t *store.Tx) error {
		var err error
		out.Setting, err = d.changeSetting(t, l, v, false)
		return err
	})
	return connect.NewResponse(out), err
}

// ResetSetting puts a setting back to its default.
func (d *Daemon) ResetSetting(ctx context.Context, req *connect.Request[v1.ResetSettingRequest]) (*connect.Response[v1.ResetSettingResponse], error) {
	l, err := d.leaf(req.Msg.Key)
	if err != nil {
		return nil, err
	}
	if err := hostOnly(ctx, l.Key); err != nil {
		return nil, err
	}
	out := &v1.ResetSettingResponse{}
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceResetSettingProcedure, out, func(t *store.Tx) error {
		var err error
		out.Setting, err = d.changeSetting(t, l, nil, true)
		return err
	})
	return connect.NewResponse(out), err
}

// hostOnlySettings may be changed only by the host's own device: each guards the host against a paired device (a
// device could otherwise turn the guard off itself), and an agent has no business changing it either.
var hostOnlySettings = map[string]bool{
	"push.allow_private_endpoints": true, // what a device's push endpoint may reach (push.go)
	"editor.command":               true, // the program OpenInEditor runs on the host (editor.go): an agent that could set it could pick what runs on the user's click
}

// agentBarredSettings may be changed by a device (the user) but never by an agent: each decides what an agent may do to the
// user's screen or machine, so an agent that could set it would grant itself the right.
var agentBarredSettings = map[string]bool{
	"app.open_views": true, // whether an agent's OpenView may switch the user's view by itself (view.go)
}

func hostOnly(ctx context.Context, key string) error {
	if agentBarredSettings[key] {
		if a := auth.ActorFrom(ctx); a == nil || IsAgent(a.Class) {
			return denied("%s may be changed only by the user, from an app or the CLI as a device", key)
		}
	}
	if !hostOnlySettings[key] {
		return nil
	}
	if a := auth.ActorFrom(ctx); a == nil || a.Id != localDeviceID {
		return denied("%s may be changed only from the host itself", key)
	}
	return nil
}
