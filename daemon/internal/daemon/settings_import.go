package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The policy file (bin/mesh's `~/.config/agent-mesh.json`, design §21) is imported once into the settings store,
// not read as a layer: the store stays the one source of truth every device, app and agent reads and writes.
// meshd imports the file it was started with (`--policy-file`) at its first start with that path (the record
// is the meta key below); `mesh import-settings` does it again on demand. Neither replaces a setting that is
// already set, unless asked to.

const policyImportMetaKey = "policy_import"

// policyImportRecord is what the meta key holds once a file was imported.
type policyImportRecord struct {
	Path     string `json:"path"`
	Sum      string `json:"sha256"`
	At       int64  `json:"at"`
	Imported int    `json:"imported"`
	Skipped  int    `json:"skipped"`
}

func readPolicyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, err
	}
	return b, nil
}

func policySum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// applyPolicy stores the file's leaves (a dry run when t is nil, the reader being a snapshot). It returns what was
// stored and what was left alone. A setting that is set stays so unless overwrite; one set to the same value is
// not listed as imported.
//
// mayChange (nil: anything) says why the caller may not change a key, as SetSetting does for the host-only ones: a key it
// refuses is left alone and listed, so a file cannot do what its caller could not.
func (d *Daemon) applyPolicy(r store.Reader, t *store.Tx, vals []settings.PolicyValue, skips []settings.PolicySkip,
	overwrite bool, mayChange func(key string) error) (*v1.ImportSettingsResponse, error) {
	out := &v1.ImportSettingsResponse{}
	for _, s := range skips {
		out.Skipped = append(out.Skipped, s.Key+": "+s.Why)
	}
	for _, pv := range vals {
		l, ok := d.schema.Lookup(pv.Key)
		if !ok {
			out.Skipped = append(out.Skipped, pv.Key+": no such setting")
			continue
		}
		if mayChange != nil {
			if err := mayChange(pv.Key); err != nil {
				why := err.Error()
				var ce *connect.Error
				if errors.As(err, &ce) {
					why = ce.Message()
				}
				out.Skipped = append(out.Skipped, pv.Key+": "+why)
				continue
			}
		}
		before, err := d.readSetting(r, l)
		if err != nil {
			return nil, err
		}
		if before.IsSet && !overwrite {
			out.Skipped = append(out.Skipped, pv.Key+": already set (--overwrite replaces it)")
			continue
		}
		val, err := structpb.NewValue(pv.Value)
		if err != nil {
			return nil, err
		}
		if before.IsSet && proto.Equal(before.Value, val) {
			continue
		}
		if t != nil {
			if _, err := d.changeSetting(t, l, pv.Value, false); err != nil {
				return nil, err
			}
		}
		out.Imported = append(out.Imported, &v1.ImportedSetting{Key: pv.Key, Value: val, From: pv.From})
	}
	return out, nil
}

// ImportSettings carries a policy file over, on demand.
func (d *Daemon) ImportSettings(ctx context.Context, req *connect.Request[v1.ImportSettingsRequest]) (*connect.Response[v1.ImportSettingsResponse], error) {
	m := req.Msg
	path := m.From
	if path == "" {
		path = d.cfg.PolicyFile
	}
	if path == "" {
		return nil, invalid("no policy file: meshd was started without --policy-file, so give --from <file>")
	}
	out := &v1.ImportSettingsResponse{}
	// What reads and applies the file. A write runs it only after d.write has looked for the mutation, so a retry
	// replays the first answer even when the file is gone or has changed since; the answer stored is complete.
	apply := func(r store.Reader, t *store.Tx) error {
		b, err := readPolicyFile(path)
		if err != nil {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("reading %s: %v", path, err))
		}
		vals, skips, err := settings.ParsePolicy(d.schema, b)
		if err != nil {
			return invalid("%s is not a policy file: %v", path, err)
		}
		res, err := d.applyPolicy(r, t, vals, skips, m.Overwrite, func(key string) error { return hostOnly(ctx, key) })
		if err != nil {
			return err
		}
		out.File, out.DryRun, out.Imported, out.Skipped = expandHome(path), m.DryRun, res.Imported, res.Skipped
		// Applying the file meshd was started with is its import: no "changed since" warning at the next start.
		if t != nil && d.cfg.PolicyFile != "" && expandHome(path) == expandHome(d.cfg.PolicyFile) {
			return d.recordPolicyImport(t, expandHome(path), b, res)
		}
		return nil
	}
	var err error
	if m.DryRun {
		err = d.store.Snapshot(ctx, func(r store.Reader) error { return apply(r, nil) })
	} else {
		_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceImportSettingsProcedure, out, func(t *store.Tx) error {
			return apply(t.Reader, t)
		})
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// recordPolicyImport notes in the store that a policy file (these bytes) was imported.
func (d *Daemon) recordPolicyImport(t *store.Tx, path string, file []byte, res *v1.ImportSettingsResponse) error {
	raw, err := json.Marshal(policyImportRecord{Path: path, Sum: policySum(file), At: t.Now().Unix(),
		Imported: len(res.Imported), Skipped: len(res.Skipped)})
	if err != nil {
		return err
	}
	return t.PutMeta(policyImportMetaKey, string(raw))
}

// importPolicyAtStart imports the file meshd was started with the first time it sees that path. A missing or
// unreadable file, or one that isn't JSON, is logged and never stops meshd (the next start tries again, since
// nothing was recorded); a file that changed since its import is only logged.
func (d *Daemon) importPolicyAtStart(ctx context.Context) {
	path := d.cfg.PolicyFile
	if path == "" {
		return
	}
	b, err := readPolicyFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			d.log.Info("no policy file to import", "path", path)
		} else {
			d.log.Warn("policy file not imported", "path", path, "err", err)
		}
		return
	}
	var rec policyImportRecord
	if raw, err := d.store.Read(ctx).Meta(policyImportMetaKey); err == nil && raw != "" && json.Unmarshal([]byte(raw), &rec) == nil && rec.Path == expandHome(path) {
		if rec.Sum != policySum(b) {
			d.log.Warn("the policy file changed since its import; `mesh import-settings` applies it (add --overwrite to replace settings already set)", "path", path)
		}
		return
	}
	vals, skips, err := settings.ParsePolicy(d.schema, b)
	if err != nil {
		d.log.Warn("policy file not imported", "path", path, "err", err)
		return
	}
	var out *v1.ImportSettingsResponse
	err = d.store.Write(ctx, nil, "", func(t *store.Tx) error {
		var err error
		if out, err = d.applyPolicy(t.Reader, t, vals, skips, false, nil); err != nil {
			return err
		}
		return d.recordPolicyImport(t, expandHome(path), b, out)
	})
	if err != nil {
		d.log.Warn("policy file not imported", "path", path, "err", err)
		return
	}
	d.log.Info("imported the policy file", "path", path, "imported", len(out.Imported), "skipped", len(out.Skipped))
	if len(out.Skipped) > 0 {
		d.log.Warn("policy file keys left alone", "skipped", strings.Join(out.Skipped, "; "))
	}
}
