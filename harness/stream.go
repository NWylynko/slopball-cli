package harness

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/nwylynko/slopball-cli/telemetry"
)

// EventKind is what a decoded agent-stream event is.
type EventKind string

const (
	// EventThinking is the model's reasoning, coalesced from token deltas.
	EventThinking EventKind = "thinking"
	// EventText is prose the agent addressed to the reader.
	EventText EventKind = "text"
	// EventTool is a concrete act on the repo — an edit, a shell command.
	EventTool EventKind = "tool"
)

// Event is one readable step of an agentic run.
//
// It exists because `--output-format text` delivers *nothing* until the process
// exits: a scaffold that runs for two minutes showed one line and then a frozen
// screen, which is indistinguishable from a hang. Every CLI has a streaming
// format; this is the one shape slopball reads them all into.
type Event struct {
	Kind EventKind
	// Tool is the verb for EventTool ("shell", "edit", "read", …), blank otherwise.
	Tool string
	// Text is the thought, the prose, or the tool's argument.
	Text string
}

// Line renders the event the way a human reads it on the console.
func (e Event) Line() string {
	switch e.Kind {
	case EventThinking:
		return "· " + e.Text
	case EventTool:
		return "▸ " + pad(e.Tool, 6) + " " + e.Text
	default:
		return e.Text
	}
}

// Activity is the one-line "what is it doing right now" for the dashboard, or
// blank for events that do not describe an act. Thinking is deliberately not an
// activity: it is the most frequent event and the least stable summary, so a
// role line driven by it would flicker faster than it could be read.
func (e Event) Activity() string {
	if e.Kind != EventTool {
		return ""
	}
	return e.Tool + " " + e.Text
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

// maxDetail bounds one rendered tool argument. A `create-next-app` invocation
// or a heredoc runs off the side of a terminal and pushes the useful part of
// the line out of view.
const maxDetail = 100

// streamDecoder turns a harness CLI's stdout into Events as it arrives. It is
// an io.Writer so it drops straight into the existing streaming plumbing.
type streamDecoder struct {
	name Name
	emit func(Event)

	buf      string
	cwd      string          // learned from the init frame; makes paths relative
	thinking []string        // open thought, accumulated from deltas
	seenCall map[string]bool // calls already rendered, by the stream's own call id
	lastTool string          // one-slot fallback for a frame that carries no id
}

func newStreamDecoder(name Name, emit func(Event)) *streamDecoder {
	return &streamDecoder{name: name, emit: emit}
}

// Write consumes whatever arrived and emits every event that is now complete.
func (d *streamDecoder) Write(p []byte) (int, error) {
	d.buf += string(p)
	for {
		nl := strings.IndexByte(d.buf, '\n')
		if nl < 0 {
			break
		}
		d.line(d.buf[:nl])
		d.buf = d.buf[nl+1:]
	}
	return len(p), nil
}

// Close flushes a trailing partial line and any thought still open — a run that
// ends mid-thought must not swallow the last thing it said.
func (d *streamDecoder) Close() {
	if rest := strings.TrimSpace(d.buf); rest != "" {
		d.line(rest)
	}
	d.buf = ""
	d.flushThinking()
}

func (d *streamDecoder) line(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	// codex exec streams plain text on its own, and anything a CLI writes to
	// stderr is plain text too. Passing an unparseable line through verbatim is
	// deliberate: a crash or a permission prompt must reach the screen, not be
	// silently dropped for failing to be JSON.
	if d.name == Codex || !strings.HasPrefix(raw, "{") {
		d.flushThinking()
		d.send(Event{Kind: EventText, Text: raw})
		return
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		d.flushThinking()
		d.send(Event{Kind: EventText, Text: raw})
		return
	}
	switch d.name {
	case Cursor:
		d.cursorFrame(frame)
	default:
		d.claudeFrame(frame)
	}
}

func (d *streamDecoder) send(e Event) {
	if strings.TrimSpace(e.Text) == "" {
		return
	}
	e.Text = truncate(strings.TrimSpace(e.Text), maxDetail)
	// The agent's own steps are their own event NAME rather than more
	// client.log rows, which is what makes them prunable later (plan 46 ticket
	// 13). Off, or on a machine that never opted in, this is a nil check.
	telemetry.EmitHarnessEvent(string(e.Kind), e.Tool, e.Text, e.Activity())
	d.emit(e)
}

// think accumulates a delta; the thought is emitted whole, because a token
// fragment per line ("Scaffolding a", " Vite React app") is unreadable.
func (d *streamDecoder) think(text string) { d.thinking = append(d.thinking, text) }

func (d *streamDecoder) flushThinking() {
	if len(d.thinking) == 0 {
		return
	}
	whole := strings.Join(d.thinking, "")
	d.thinking = nil
	for _, part := range strings.Split(whole, "\n") {
		d.send(Event{Kind: EventThinking, Text: part})
	}
}

// tool emits one act, unless it repeats a call already rendered — cursor reports
// every call twice (`started`, then `completed`) and the second says nothing new.
//
// The identity is the stream's own call id, and it has to be: with more than one
// call in flight the two frames are not adjacent, so a "same as the line before"
// check rendered whole runs of work twice on screen. Keying on verb+detail
// instead would swallow a genuine second `npm run build`, which is real progress.
func (d *streamDecoder) tool(id, verb, detail string) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return
	}
	if id != "" {
		if d.seenCall[id] {
			return
		}
		if d.seenCall == nil {
			d.seenCall = map[string]bool{}
		}
		d.seenCall[id] = true
	} else {
		// No id to key on. Fall back to the adjacent check, which stays one-slot
		// for the reason above: a repeat that is not adjacent is a real act.
		key := verb + "\x00" + detail
		if key == d.lastTool {
			return
		}
		d.lastTool = key
	}
	d.flushThinking()
	d.send(Event{Kind: EventTool, Tool: verb, Text: detail})
}

// rel shortens a path against the work tree the agent was given.
func (d *streamDecoder) rel(path string) string {
	if path == "" {
		return ""
	}
	if d.cwd != "" {
		if r := strings.TrimPrefix(path, strings.TrimSuffix(d.cwd, "/")+"/"); r != path {
			return r
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Base(path)
	}
	return path
}

// shorten rewrites the work tree out of a shell command, so what survives the
// truncation is what the command actually does.
func (d *streamDecoder) shorten(cmd string) string {
	if d.cwd == "" {
		return cmd
	}
	root := strings.TrimSuffix(d.cwd, "/")
	cmd = strings.ReplaceAll(cmd, root+"/", "")
	return strings.ReplaceAll(cmd, root, ".")
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// --- cursor -----------------------------------------------------------------

// cursorFrame reads `cursor-agent -p --output-format stream-json`. Thinking
// arrives as token deltas closed by a `completed`, tool calls carry exactly one
// `<verb>ToolCall` object, and everything else is session bookkeeping.
func (d *streamDecoder) cursorFrame(frame map[string]json.RawMessage) {
	typ, sub := str(frame["type"]), str(frame["subtype"])
	switch typ {
	case "system":
		if cwd := str(frame["cwd"]); cwd != "" {
			d.cwd = cwd
		}
	case "thinking":
		if sub == "delta" {
			d.think(str(frame["text"]))
			return
		}
		d.flushThinking()
	case "assistant":
		d.flushThinking()
		for _, t := range messageText(frame["message"]) {
			d.send(Event{Kind: EventText, Text: t})
		}
	case "tool_call":
		var call map[string]json.RawMessage
		if err := json.Unmarshal(frame["tool_call"], &call); err != nil {
			return
		}
		for key, body := range call {
			verb := strings.TrimSuffix(key, "ToolCall")
			if verb == key {
				continue // hookAdvice and friends are not calls
			}
			var wrap struct {
				Args map[string]json.RawMessage `json:"args"`
			}
			if err := json.Unmarshal(body, &wrap); err != nil {
				continue
			}
			d.tool(str(frame["call_id"]), cursorVerb(verb), d.detail(wrap.Args))
			return
		}
	}
	// "user" echoes the prompt back and "result" repeats the final message; both
	// are bookkeeping, not progress.
}

// cursorVerb maps cursor's camelCase tool names onto the short verbs shown on
// screen. Anything unmapped keeps its own name rather than being hidden — a new
// tool should read oddly, never invisibly.
func cursorVerb(v string) string {
	switch v {
	case "shell":
		return "shell"
	case "edit", "write":
		return "edit"
	case "read":
		return "read"
	case "ls", "glob":
		return "list"
	case "grep", "search", "semSearch":
		return "search"
	case "delete", "rm":
		return "delete"
	default:
		return strings.ToLower(v)
	}
}

// --- claude -----------------------------------------------------------------

// claudeFrame reads `claude -p --output-format stream-json --verbose`: the
// Anthropic message shape, where a tool call is a content block rather than its
// own frame type.
func (d *streamDecoder) claudeFrame(frame map[string]json.RawMessage) {
	switch str(frame["type"]) {
	case "system":
		if cwd := str(frame["cwd"]); cwd != "" {
			d.cwd = cwd
		}
	case "assistant":
		var msg struct {
			Content []struct {
				Type     string                     `json:"type"`
				Text     string                     `json:"text"`
				Thinking string                     `json:"thinking"`
				ID       string                     `json:"id"`
				Name     string                     `json:"name"`
				Input    map[string]json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal(frame["message"], &msg); err != nil {
			return
		}
		for _, block := range msg.Content {
			switch block.Type {
			case "thinking":
				d.think(block.Thinking)
			case "text":
				d.flushThinking()
				d.send(Event{Kind: EventText, Text: block.Text})
			case "tool_use":
				d.tool(block.ID, strings.ToLower(block.Name), d.detail(block.Input))
			}
		}
	}
	// "user" carries tool results, "rate_limit_event" is metering, and the final
	// object has no type at all — none of them are progress.
}

// --- shared -----------------------------------------------------------------

// detail picks the one argument that says what a tool call is doing. The order
// is by how much it tells a reader, not by how common it is.
func (d *streamDecoder) detail(args map[string]json.RawMessage) string {
	for _, key := range []string{"command", "cmd", "file_path", "path", "filePath", "notebook_path"} {
		if v := str(args[key]); v != "" {
			if key == "command" || key == "cmd" {
				// The work tree is a temp path the reader did not choose and
				// cannot act on, and it appears several times in one command —
				// `cd /tmp/slopball-setup-3108460813 && npm install` spends half
				// the visible line saying where, and gets truncated before what.
				return d.shorten(v)
			}
			return d.rel(v)
		}
	}
	for _, key := range []string{"pattern", "query", "url", "prompt", "description"} {
		if v := str(args[key]); v != "" {
			return v
		}
	}
	return ""
}

// messageText pulls the text blocks out of a `message` object.
func messageText(raw json.RawMessage) []string {
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var out []string
	for _, b := range msg.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// str reads a JSON value as a string, tolerating a non-string (a number, an
// object) by answering blank rather than failing the whole frame.
func str(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

var _ io.Writer = (*streamDecoder)(nil)
