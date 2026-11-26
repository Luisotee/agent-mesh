// Command fakeopencode stands in for OpenCode (`opencode`) in tests. It supports `opencode models --verbose`,
// logs launches to $FAKEOPENCODE_ARGV_LOG, reads OPENCODE_CONFIG_CONTENT / OPENCODE_PERMISSION, draws
// OpenCode's `┃` gutter input box (with optional placeholder and wrapped status rows), writes OpenCode-compatible
// SQLite rows into $AGENT_MESH_OPENCODE_DB (or XDG_DATA_HOME/opencode/opencode.db), and acts out turns from
// lines typed into its terminal:
//
//	any text       inserts user + completed assistant message in opencode.db
//	/hold          inserts user + open assistant message, waits for a line, then completes
//	/edit          creates blocked.txt unless OPENCODE_PERMISSION denies edits
//	/push          prints "pushed" unless OPENCODE_PERMISSION denies git push
//	/limit-free    writes FreeUsageLimitError reply and prints retry banner
//	/limit-429     writes 429 reply
//	/limit-pane    writes open assistant reply and prints [retrying ... attempt #4] banner
//	/unavailable   writes "Upstream request failed: Model is unavailable." reply (status 400)
//	/error         writes 500 error reply
//	/exit          exits 0
//	/crash         exits 7
package main

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const defaultModels = `opencode/big-pickle
{
  "id": "big-pickle",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
  "capabilities": {"toolcall": true},
  "release_date": "2026-11-20",
  "variants": {"low": {"reasoningEffort": "low"}, "medium": {"reasoningEffort": "medium"}, "high": {"reasoningEffort": "high"}}
}
opencode/deepseek-v4.1-flash-free
{
  "id": "deepseek-v4.1-flash-free",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
  "capabilities": {"toolcall": true},
  "release_date": "2026-11-10",
  "variants": {"low": {"reasoningEffort": "low"}, "high": {"reasoningEffort": "high"}}
}
opencode/flint-40b-free
{
  "id": "flint-40b-free",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
  "capabilities": {"toolcall": true},
  "release_date": "2026-10-20",
  "variants": {"low": {"reasoningEffort": "low"}, "high": {"reasoningEffort": "high"}}
}
opencode/beam-free
{
  "id": "beam-free",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
  "capabilities": {"toolcall": true},
  "release_date": "2026-10-05",
  "variants": {"low": {"reasoningEffort": "low"}, "high": {"reasoningEffort": "high"}}
}
opencode/tidepool-alpha-free
{
  "id": "tidepool-alpha-free",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
  "capabilities": {"toolcall": true},
  "release_date": "2026-09-01",
  "variants": {"high": {"reasoningEffort": "high"}}
}
`

var (
	sessionID string
	agentName string
	model     string
	cwd, _    = os.Getwd()
	seq       atomic.Int64
	lastMs    atomic.Int64
)

func nextMs() int64 {
	now := time.Now().UnixMilli()
	for {
		prev := lastMs.Load()
		next := now
		if next <= prev {
			next = prev + 1
		}
		if lastMs.CompareAndSwap(prev, next) {
			return next
		}
	}
}

func main() {
	args := os.Args[1:]
	if len(args) >= 1 && args[0] == "models" {
		if p := os.Getenv("FAKEOPENCODE_MODELS_FILE"); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				fmt.Print(string(b))
				return
			}
		}
		if s := os.Getenv("FAKEOPENCODE_MODELS"); s != "" {
			fmt.Print(s)
			return
		}
		fmt.Print(defaultModels)
		return
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--agent":
			if i+1 < len(args) {
				i++
				agentName = args[i]
			}
		case "-m", "--model":
			if i+1 < len(args) {
				i++
				model = args[i]
			}
		case "-s", "--session":
			if i+1 < len(args) {
				i++
				sessionID = args[i]
			}
		}
	}

	if logPath := os.Getenv("FAKEOPENCODE_ARGV_LOG"); logPath != "" {
		rec := map[string]any{
			"argv":       args,
			"agent":      agentName,
			"model":      model,
			"session":    sessionID,
			"config":     os.Getenv("OPENCODE_CONFIG_CONTENT"),
			"permission": os.Getenv("OPENCODE_PERMISSION"),
			"autoupdate": os.Getenv("OPENCODE_DISABLE_AUTOUPDATE"),
		}
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			b, _ := json.Marshal(rec)
			f.Write(append(b, '\n'))
			f.Close()
		}
	}

	if d, err := time.ParseDuration(os.Getenv("FAKEOPENCODE_START_DELAY")); err == nil && d > 0 {
		time.Sleep(d)
	}
	fmt.Printf("fake opencode %s (%s)\r\n", agentName, model)
	if d, err := time.ParseDuration(os.Getenv("FAKEOPENCODE_DRAW_FOR")); err == nil && d > 0 {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			fmt.Print(".")
		}
	}
	prompt()

	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := strings.TrimSpace(strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(in.Text()))
		if line == "" {
			prompt()
			continue
		}
		if line == "/exit" {
			fmt.Print("bye\r\n")
			return
		}
		if line == "/crash" {
			os.Exit(7)
		}

		db := openDB()
		if sessionID == "" {
			sessionID = "ses_" + newHex(6)
		}
		ensureSession(db, sessionID)
		userMsgID := "msg_" + newHex(8)
		writeUserMessage(db, sessionID, userMsgID, line)

		providerID, modelID, _ := strings.Cut(model, "/")
		asstID := "msg_" + newHex(8)
		created := writeAssistantMessage(db, sessionID, asstID, providerID, modelID, 0, "", nil)

		switch line {
		case "/hold":
			fmt.Print("holding\r\n")
			in.Scan()
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "stop", nil)
			fmt.Print("resumed\r\n")
		case "/edit":
			if editDenied() {
				fmt.Print("edit denied\r\n")
			} else {
				_ = os.WriteFile(filepath.Join(cwd, "blocked.txt"), []byte("hi"), 0o600)
				fmt.Print("wrote blocked.txt\r\n")
			}
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "stop", nil)
		case "/push":
			if bashDenied("git push origin HEAD") {
				fmt.Print("git push denied\r\n")
			} else {
				fmt.Print("pushed\r\n")
			}
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "stop", nil)
		case "/limit-free":
			errObj := map[string]any{
				"name": "APIError",
				"data": map[string]any{
					"statusCode":   429,
					"message":      "Free usage exceeded, subscribe to Go",
					"responseBody": `{"error":"FreeUsageLimitError"}`,
				},
			}
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "", errObj)
			fmt.Print("  \u2503  Free usage exceeded, subscribe to Go [retrying in 2s attempt #1]\r\n")
		case "/limit-429":
			errObj := map[string]any{
				"name": "APIError",
				"data": map[string]any{
					"statusCode": 429,
					"message":    "Too Many Requests",
				},
			}
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "", errObj)
			fmt.Print("  \u2503  Too Many Requests\r\n")
		case "/limit-pane":
			fmt.Print("  \u2503  Too Many Requests [retrying in 30s attempt #4]\r\n")
		case "/unavailable":
			errObj := map[string]any{
				"name": "APIError",
				"data": map[string]any{
					"statusCode":   400,
					"message":      "Upstream request failed: Model is unavailable.",
					"responseBody": `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`,
				},
			}
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "", errObj)
			fmt.Print("  \u2503  Upstream request failed: Model is unavailable.\r\n")
		case "/error":
			errObj := map[string]any{
				"name": "APIError",
				"data": map[string]any{
					"statusCode": 500,
					"message":    "internal server error",
				},
			}
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "", errObj)
			fmt.Print("error: internal server error\r\n")
		default:
			writeAssistantMessage(db, sessionID, asstID, providerID, modelID, created, "stop", nil)
			fmt.Printf("done: %s\r\n", line)
		}
		if db != nil {
			db.Close()
		}
		prompt()
	}
}

func prompt() {
	const gut = "\x1b[0m\x1b[38;2;255;255;255m\x1b[48;2;10;10;10m  \x1b[0m\x1b[38;2;245;167;66m\x1b[48;2;10;10;10m\u2503\x1b[0m"
	const bottom = "\x1b[0m\x1b[38;2;255;255;255m\x1b[48;2;10;10;10m  \x1b[0m\x1b[38;2;245;167;66m\u2579\x1b[0m\x1b[38;2;30;30;30m\u2580\u2580\u2580\u2580\x1b[0m"
	fmt.Printf("\r\n%s  \r\n", gut)
	if os.Getenv("FAKEOPENCODE_PLACEHOLDER") != "" {
		fmt.Printf("%s\x1b[48;2;30;30;30m  \x1b[0m\x1b[38;2;128;128;128m\x1b[48;2;30;30;30mAsk anything\u2026 \"Fix a TODO in the codebase\"\x1b[0m\r\n", gut)
	} else {
		fmt.Printf("%s  \r\n", gut)
	}
	fmt.Printf("%s  \r\n", gut)
	if os.Getenv("FAKEOPENCODE_STATUS_WRAP") != "" {
		for _, row := range []string{
			"Mesh-    \u00b7DeepSeek V4.1 Flash Free Op\u00b7 h",
			"Abc123                             en  i",
			"                                   Co  g",
			"                                   de  h",
			"",
			"",
		} {
			if row == "" {
				fmt.Printf("%s  \r\n", gut)
			} else {
				fmt.Printf("%s\x1b[48;2;30;30;30m  \x1b[0m\x1b[38;2;128;128;128m%s\x1b[0m\r\n", gut, row)
			}
		}
	} else {
		fmt.Printf("%s\x1b[48;2;30;30;30m  \x1b[0m\x1b[38;2;245;167;66m%s\x1b[0m \x1b[38;2;128;128;128m%s auto\x1b[0m\r\n", gut, agentName, model)
	}
	fmt.Printf("%s\r\n", bottom)
}

func editDenied() bool {
	raw := os.Getenv("OPENCODE_PERMISSION")
	if raw == "" {
		return false
	}
	var p struct {
		Edit map[string]string `json:"edit"`
	}
	if json.Unmarshal([]byte(raw), &p) != nil {
		return true
	}
	return p.Edit["*"] == "deny"
}

func bashDenied(cmd string) bool {
	raw := os.Getenv("OPENCODE_PERMISSION")
	if raw == "" {
		return false
	}
	var p struct {
		Bash map[string]string `json:"bash"`
	}
	if json.Unmarshal([]byte(raw), &p) != nil {
		return true
	}
	for pat, act := range p.Bash {
		if act == "deny" && strings.HasPrefix(cmd, strings.TrimSuffix(pat, "*")) {
			return true
		}
	}
	return false
}

func dbPath() string {
	if p := os.Getenv("AGENT_MESH_OPENCODE_DB"); p != "" {
		return p
	}
	if p := os.Getenv("MESH_OPENCODE_DB"); p != "" {
		return p
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		if home := os.Getenv("HOME"); home != "" {
			base = filepath.Join(home, ".local", "share")
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "opencode", "opencode.db")
}

func openDB() *sql.DB {
	p := dbPath()
	if p == "" {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	db, err := sql.Open("sqlite", "file:"+p+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil
	}
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, parent_id TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
	`)
	return db
}

func ensureSession(db *sql.DB, sid string) {
	if db == nil {
		return
	}
	now := nextMs()
	_, _ = db.Exec(`INSERT OR IGNORE INTO session (id, directory, parent_id, time_created, time_updated) VALUES (?, ?, NULL, ?, ?)`,
		sid, cwd, now, now)
	_, _ = db.Exec(`UPDATE session SET time_updated = ? WHERE id = ?`, now, sid)
}

func writeUserMessage(db *sql.DB, sid, mid, text string) {
	if db == nil {
		return
	}
	now := nextMs()
	data, _ := json.Marshal(map[string]any{"role": "user", "time": map[string]any{"created": now}})
	_, _ = db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		mid, sid, now, now, string(data))
	pdata, _ := json.Marshal(map[string]any{"type": "text", "text": text})
	_, _ = db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		"prt_"+newHex(8), mid, sid, now, now, string(pdata))
	_, _ = db.Exec(`UPDATE session SET time_updated = ? WHERE id = ?`, now, sid)
}

func writeAssistantMessage(db *sql.DB, sid, mid, providerID, modelID string, created int64, finish string, errObj map[string]any) int64 {
	if db == nil {
		return 0
	}
	now := nextMs()
	if created == 0 {
		created = now
	}
	t := map[string]any{"created": created}
	if finish != "" || errObj != nil {
		t["completed"] = now
	}
	msg := map[string]any{
		"role":       "assistant",
		"providerID": providerID,
		"modelID":    modelID,
		"time":       t,
	}
	if finish != "" {
		msg["finish"] = finish
	}
	if errObj != nil {
		msg["error"] = errObj
	}
	data, _ := json.Marshal(msg)
	_, _ = db.Exec(`INSERT OR REPLACE INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		mid, sid, created, now, string(data))
	_, _ = db.Exec(`UPDATE session SET time_updated = ? WHERE id = ?`, now, sid)
	return created
}

func newHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", seq.Add(1))
	}
	return fmt.Sprintf("%x", b)
}
