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
func (d *Daemon) applyPolicy(r store.Reader, t *store.Tx, vals []settings.PolicyValue, skips []settings.PolicySkip,
	overwrite bool) (*v1.ImportSettingsResponse, error) {
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
	b, err := readPolicyFile(path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("reading %s: %v", path, err))
	}
	vals, skips, err := settings.ParsePolicy(d.schema, b)
	if err != nil {
		return nil, invalid("%s is not a policy file: %v", path, err)
	}
	out := &v1.ImportSettingsResponse{}
	if m.DryRun {
		err = d.store.Snapshot(ctx, func(r store.Reader) error {
			var err error
			out, err = d.applyPolicy(r, nil, vals, skips, m.Overwrite)
			return err
		})
	} else {
		// d.write stores `out` as the mutation's response, so the result goes into it (a retry replays it).
		_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceImportSettingsProcedure, out, func(t *store.Tx) error {
			res, err := d.applyPolicy(t.Reader, t, vals, skips, m.Overwrite)
			if err != nil {
				return err
			}
			out.Imported, out.Skipped = res.Imported, res.Skipped
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	out.File, out.DryRun = expandHome(path), m.DryRun
	return connect.NewResponse(out), nil
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
		if out, err = d.applyPolicy(t.Reader, t, vals, skips, false); err != nil {
			return err
		}
		raw, err := json.Marshal(policyImportRecord{Path: expandHome(path), Sum: policySum(b), At: t.Now().Unix(),
			Imported: len(out.Imported), Skipped: len(out.Skipped)})
		if err != nil {
			return err
		}
		return t.PutMeta(policyImportMetaKey, string(raw))
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
