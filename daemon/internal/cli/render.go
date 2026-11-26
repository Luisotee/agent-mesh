package cli

import (
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// A few commands are read by agents whose context is the scarce thing (an inbox, the policy): their response
// prints as short text by default, and `--format json` prints the whole response as every other command does.
// This is the CLI's own view of the response: the API and the other clients are untouched.
var renderers = map[string]struct {
	into func() proto.Message
	text func(proto.Message, io.Writer)
}{
	"inbox":    {func() proto.Message { return &v1.ReadInboxResponse{} }, renderInbox},
	"settings": {func() proto.Message { return &v1.ListSettingsResponse{} }, renderSettings},
}

// hasText reports whether a command has a text form (and so a --format flag).
func hasText(cli string) bool { _, ok := renderers[cli]; return ok }

// renderText prints res, a dynamic response, in its command's text form.
func renderText(cli string, res proto.Message, w io.Writer) error {
	r := renderers[cli]
	raw, err := proto.Marshal(res)
	if err != nil {
		return err
	}
	typed := r.into()
	if err := proto.Unmarshal(raw, typed); err != nil {
		return err
	}
	r.text(typed, w)
	return nil
}

// renderInbox prints each message as a header line, then its body: who, what kind, when, and its id.
func renderInbox(m proto.Message, w io.Writer) {
	msgs := m.(*v1.ReadInboxResponse).Messages
	if len(msgs) == 0 {
		fmt.Fprintln(w, "no messages")
		return
	}
	for i, msg := range msgs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "── %s from %s at %s (%s) ──\n", enumLower(msg.Type, "MESSAGE_TYPE_"), msg.From.GetName(),
			msg.CreatedAt.AsTime().UTC().Format("2006-01-02T15:04:05Z"), msg.Id)
		fmt.Fprintln(w, strings.TrimRight(msg.Body, "\n"))
	}
}

// renderSettings prints one `key = value` line per setting, and the default after an override.
func renderSettings(m proto.Message, w io.Writer) {
	settings := m.(*v1.ListSettingsResponse).Settings
	if len(settings) == 0 {
		fmt.Fprintln(w, "no settings")
		return
	}
	for _, s := range settings {
		line := s.Key + " = " + jsonOf(s.Value)
		if s.IsSet {
			line += "   (set; default " + jsonOf(s.Default) + ")"
		}
		fmt.Fprintln(w, line)
	}
}

func jsonOf(m proto.Message) string {
	if m == nil || !m.ProtoReflect().IsValid() {
		return "null"
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		return "?"
	}
	return string(b)
}

func enumLower(e interface{ String() string }, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(e.String(), prefix))
}
