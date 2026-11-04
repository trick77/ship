// Package mcpserver is a small stateless MCP server over streamable HTTP, JSON responses only: no session, no GET
// stream. It answers the 2026-07-28 revision and the initialize-era ones. A caller describes its tools in a Server
// and mounts New's handler; the protocol, the caps and the result shape are this package's.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxBodyBytes bounds one request body; tool arguments are a few strings.
	maxBodyBytes = 1 << 20
	// defaultToolTimeout bounds one tool call: a calling client gives up at 30 s, this leaves room to still deliver
	// a degraded answer or an error inside that.
	defaultToolTimeout = 25 * time.Second
	// defaultMaxTextBytes caps one tool's text. Clients cut at 32 KB; staying under keeps the cut ours, at a line.
	defaultMaxTextBytes = 24 * 1024
	// defaultToolsTTL is how long a client may cache tools/list and discovery: a static tool set, an hour.
	defaultToolsTTL = time.Hour
	// maxFilesBytes bounds one result's files together, so a reply stays under the 4 MiB a client caps it at
	// (loom does) and the text always arrives.
	maxFilesBytes = 3 << 20
)

// Server describes one MCP endpoint. Zero values are the defaults noted per field.
type Server struct {
	Name    string // serverInfo.name, and the server field of the tool call log
	Version string // serverInfo.version
	// Instructions is the usage hint sent on initialize and discovery.
	Instructions string
	// InstructionsFunc, when set, replaces Instructions and is built per request: a hint naming the user's data
	// (peeq's channel list) changes without a restart. Cache inside it; discovery and initialize both call it.
	InstructionsFunc func(ctx context.Context) string
	// Tools in tools/list order.
	Tools []Tool
	// Aliases maps retired tool names to current ones. They answer calls but are never listed: a client that cached
	// the old tools/list keeps working until it refreshes.
	Aliases map[string]string
	// ToolsTTL is how long a 2026-07-28 client may cache tools/list and discovery; 0 is an hour. A tool set that
	// changes per user (InstructionsFunc, DescriptionFunc) wants it short.
	ToolsTTL time.Duration
	// ToolTimeout bounds one tool call; 0 is 25 s.
	ToolTimeout time.Duration
	// MaxTextBytes caps one tool's text, cut at a line and marked; 0 is 24 KiB.
	MaxTextBytes int
	// AllowedOrigin is a base URL whose scheme://host[:port] is the one browser Origin accepted. Empty refuses
	// every request that carries an Origin: server-to-server callers send none.
	AllowedOrigin string
	// Auth wraps the protocol handler after the origin check, so a page on a foreign origin learns nothing, not
	// even whether a guessed token was right. Nil leaves auth to whatever the handler is mounted behind.
	Auth func(http.Handler) http.Handler
	// UserHeader names a request header carrying the caller's user id, put in the tool's context (User). Only
	// trustworthy behind auth that only the sending service passes. Empty: no user.
	UserHeader string
}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Title       string // display name, sent top-level and as annotations.title
	Description string
	// DescriptionFunc, when set, replaces Description in tools/list, built per request.
	DescriptionFunc func(ctx context.Context) string
	Schema          map[string]any // input JSON schema; ObjectSchema builds the usual closed object
	ReadOnly        bool           // annotations.readOnlyHint
	OpenWorld       bool           // annotations.openWorldHint: the tool reaches beyond the server's own data
	Run             func(ctx context.Context, args json.RawMessage) Result
}

// Source is one citable link in a tool result's structuredContent. Image is a preview picture.
type Source struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Label   string `json:"label,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Image   string `json:"image,omitempty"`
}

// File is data a tool hands the client as an MCP embedded resource (loom gives it to run_python for the turn).
type File struct {
	Name     string // a plain file name, e.g. strava_123_streams.csv
	MimeType string // text/csv or application/json
	Text     string
}

// Result is what a tool produced. Sources[i] belongs to the text's item i+1; nil Sources sends no
// structuredContent, a non-nil empty slice an empty list.
type Result struct {
	Text    string
	Sources []Source
	Files   []File
	IsError bool
}

// ErrorResult is a failed call the model reads and explains.
func ErrorResult(msg string) Result { return Result{Text: msg, IsError: true} }

type userKey struct{}

// WithUser returns ctx carrying a user id, as the handler does from Server.UserHeader.
func WithUser(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

// User is the user id a call is made for; "" when none was sent.
func User(ctx context.Context) string {
	id, _ := ctx.Value(userKey{}).(string)
	return id
}

// DecodeArgs unmarshals tool arguments strictly, so a misspelt field is an error the model can read rather than
// a silently ignored filter.
func DecodeArgs(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// ObjectSchema is a closed JSON object schema.
func ObjectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

type handler struct {
	s       Server
	origin  string
	tools   map[string]Tool
	ttlMs   int64
	timeout time.Duration
	maxText int
}

// New returns the endpoint's handler: origin check, then Auth, then method, size and JSON-RPC handling.
func New(s Server) http.Handler {
	h := &handler{s: s, origin: originOf(s.AllowedOrigin), tools: map[string]Tool{},
		ttlMs: s.ToolsTTL.Milliseconds(), timeout: s.ToolTimeout, maxText: s.MaxTextBytes}
	if h.ttlMs <= 0 {
		h.ttlMs = defaultToolsTTL.Milliseconds()
	}
	if h.timeout <= 0 {
		h.timeout = defaultToolTimeout
	}
	if h.maxText <= 0 {
		h.maxText = defaultMaxTextBytes
	}
	for _, t := range s.Tools {
		h.tools[t.Name] = t
	}
	var inner http.Handler = http.HandlerFunc(h.serve)
	if s.Auth != nil {
		inner = s.Auth(inner)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Before auth: DNS rebinding included, a foreign page gets nothing.
		if o := r.Header.Get("Origin"); o != "" && (h.origin == "" || !strings.EqualFold(o, h.origin)) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// originOf reduces a base URL to scheme://host[:port], or "" when it is not an absolute URL.
func originOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Stateless and JSON-only: no GET stream to open, no session to DELETE.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "could not read the request body", nil)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, "batch requests are not supported", nil)
		return
	}
	var req request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeParseError, "parse error", nil)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, codeInvalidRequest, "invalid request", nil)
		return
	}
	h.dispatch(w, r, req)
}

func (h *handler) instructions(ctx context.Context) string {
	if h.s.InstructionsFunc != nil {
		return h.s.InstructionsFunc(ctx)
	}
	return h.s.Instructions
}

func (h *handler) toolDefs(ctx context.Context) []map[string]any {
	out := make([]map[string]any, 0, len(h.s.Tools))
	for _, t := range h.s.Tools {
		desc := t.Description
		if t.DescriptionFunc != nil {
			desc = t.DescriptionFunc(ctx)
		}
		// The title goes out twice: top-level for the current spec, annotations.title for clients that still read
		// the older place. Dropping either loses the title for those clients.
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": desc,
			"inputSchema": t.Schema,
			"annotations": map[string]any{"title": t.Title, "readOnlyHint": t.ReadOnly, "openWorldHint": t.OpenWorld},
		})
	}
	return out
}

func (h *handler) callTool(w http.ResponseWriter, r *http.Request, req request, modern bool) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "tools/call needs a tool name", nil)
		return
	}
	t, ok := h.tools[p.Name]
	if !ok {
		t, ok = h.tools[h.s.Aliases[p.Name]]
	}
	if !ok {
		// -32602 on HTTP 200: a dual-era client reads any other 4xx as "legacy server" and falls back.
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "unknown tool: "+p.Name, nil)
		return
	}
	if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
		p.Arguments = json.RawMessage("{}")
	}
	ctx := r.Context()
	if h.s.UserHeader != "" {
		ctx = WithUser(ctx, strings.TrimSpace(r.Header.Get(h.s.UserHeader)))
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	start := time.Now()
	res := t.Run(ctx, p.Arguments)
	slog.Info("mcp tool call", "server", h.s.Name, "tool", p.Name, "is_error", res.IsError,
		"ms", time.Since(start).Milliseconds())

	files, dropped := fileContent(res.Files)
	text := res.Text
	if len(dropped) > 0 {
		// Capped first, so the note survives a long text: without it the model would ask for a file it never got.
		note := "\n(not attached, too large: " + strings.Join(dropped, ", ") + ")"
		text = CapText(text, h.maxText-len(note)) + note
	} else {
		text = CapText(text, h.maxText)
	}
	content := []map[string]any{{"type": "text", "text": text}}
	result := map[string]any{"content": append(content, files...)}
	if res.Sources != nil {
		result["structuredContent"] = map[string]any{"sources": res.Sources}
	}
	if res.IsError {
		result["isError"] = true
	}
	if modern {
		result = h.modern(result)
	}
	writeResult(w, req.ID, result)
}

// fileContent turns files into embedded resources, in order, while they fit maxFilesBytes together; the names of
// those that do not are returned so the text can say so.
func fileContent(files []File) ([]map[string]any, []string) {
	var out []map[string]any
	var dropped []string
	used := 0
	for _, f := range files {
		if used+len(f.Text) > maxFilesBytes {
			dropped = append(dropped, f.Name)
			continue
		}
		used += len(f.Text)
		out = append(out, map[string]any{"type": "resource", "resource": map[string]any{
			"uri": "file:///" + f.Name, "mimeType": f.MimeType, "text": f.Text,
		}})
	}
	return out, dropped
}
