package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func echoTool() Tool {
	return Tool{
		Name: "echo", Title: "Echo", Description: "echoes", ReadOnly: true,
		Schema: ObjectSchema(map[string]any{"text": map[string]any{"type": "string"}}, "text"),
		Run: func(_ context.Context, args json.RawMessage) Result {
			var a struct {
				Text string `json:"text"`
			}
			if err := DecodeArgs(args, &a); err != nil {
				return ErrorResult("bad args: " + err.Error())
			}
			return Result{Text: a.Text, Sources: []Source{{URL: "https://x.example/" + a.Text, Title: a.Text}}}
		},
	}
}

func testHandler() http.Handler {
	return New(Server{Name: "t", Version: "v1", Instructions: "hello", Tools: []Tool{
		echoTool(),
		{Name: "big", OpenWorld: true, Run: func(context.Context, json.RawMessage) Result {
			return Result{Text: strings.Repeat("line of text\n", 4000)}
		}},
	}})
}

type rpcReply struct {
	Result map[string]any `json:"result"`
	Error  *struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	} `json:"error"`
}

func call(t *testing.T, h http.Handler, body string, headers map[string]string) (int, rpcReply, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r rpcReply
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r, rec.Body.String()
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`

func modernHeaders(method, name string) map[string]string {
	m := map[string]string{"MCP-Protocol-Version": ModernProtocolVersion, "Mcp-Method": method}
	if name != "" {
		m["Mcp-Name"] = name
	}
	return m
}

func toolText(t *testing.T, r rpcReply) string {
	t.Helper()
	content, _ := r.Result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %+v", r)
	}
	s, _ := content[0].(map[string]any)["text"].(string)
	return s
}

func TestHandler_transportRules(t *testing.T) {
	h := testHandler()
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Errorf("%s: %d Allow %q", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if code, _, _ := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("origin: %d", code)
	}
	if code, r, _ := call(t, h, `{nope`, nil); code != http.StatusBadRequest || r.Error.Code != codeParseError {
		t.Errorf("parse: %d %+v", code, r.Error)
	}
	if code, r, _ := call(t, h, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, nil); code != http.StatusBadRequest || r.Error.Code != codeInvalidRequest {
		t.Errorf("batch: %d", code)
	}
	if code, r, _ := call(t, h, `{"jsonrpc":"1.0","id":1,"method":"ping"}`, nil); code != http.StatusBadRequest || r.Error.Code != codeInvalidRequest {
		t.Errorf("version: %d", code)
	}
	if code, r, _ := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping","params":[1]}`, nil); code != http.StatusOK || r.Error.Code != codeInvalidParams {
		t.Errorf("params array: %d", code)
	}
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"p":"` + strings.Repeat("x", maxBodyBytes) + `"}}`
	if code, _, _ := call(t, h, big, nil); code != http.StatusRequestEntityTooLarge {
		t.Errorf("size: %d", code)
	}
}

func TestHandler_responsesEchoTheID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"a","method":"ping"}`))
	rec := httptest.NewRecorder()
	testHandler().ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"id":"a"`) {
		t.Errorf("body %s", rec.Body.String())
	}
}

func TestHandler_allowedOriginAndAuthOrder(t *testing.T) {
	authed := 0
	h := New(Server{Name: "t", AllowedOrigin: "https://app.example.com/base/", Auth: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authed++
			if r.Header.Get("Authorization") != "Bearer ok" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if code, _, _ := call(t, h, ping, map[string]string{"Origin": "https://evil.example", "Authorization": "Bearer nope"}); code != http.StatusForbidden || authed != 0 {
		t.Errorf("foreign origin: %d, auth ran %d times (want 403 before auth)", code, authed)
	}
	if code, _, _ := call(t, h, ping, map[string]string{"Origin": "https://APP.example.com", "Authorization": "Bearer ok"}); code != http.StatusOK {
		t.Errorf("own origin: %d", code)
	}
	if code, _, _ := call(t, h, ping, map[string]string{"Authorization": "Bearer ok"}); code != http.StatusOK {
		t.Errorf("no origin: %d", code)
	}
	if code, _, _ := call(t, h, ping, map[string]string{"Authorization": "Bearer nope"}); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", code)
	}
	if originOf("not a url") != "" || originOf("http://h:8080/x") != "http://h:8080" {
		t.Error("originOf")
	}
}

func TestHandler_legacyFlow(t *testing.T) {
	h := testHandler()
	_, r, raw := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	info, _ := r.Result["serverInfo"].(map[string]any)
	if r.Result["protocolVersion"] != "2025-06-18" || r.Result["instructions"] != "hello" || info["name"] != "t" || info["version"] != "v1" {
		t.Fatalf("initialize: %+v", r.Result)
	}
	if _, ok := r.Result["resultType"]; ok || strings.Contains(raw, "Mcp-Session-Id") {
		t.Error("a legacy result carries modern fields")
	}
	_, r, _ = call(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil)
	if r.Result["protocolVersion"] != defaultLegacyVersion {
		t.Errorf("unknown version answered %v", r.Result["protocolVersion"])
	}
	if code, _, raw := call(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); code != http.StatusAccepted || raw != "" {
		t.Errorf("notification: %d %q", code, raw)
	}
	if _, r, _ := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil); r.Result == nil || len(r.Result) != 0 {
		t.Errorf("ping: %+v", r)
	}
	_, r, _ = call(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	tools, _ := r.Result["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list: %+v", r.Result)
	}
	for _, k := range []string{"resultType", "ttlMs", "cacheScope"} {
		if _, ok := r.Result[k]; ok {
			t.Errorf("legacy tools/list carries %s", k)
		}
	}
	_, _, raw = call(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`, nil)
	if !strings.Contains(raw, `"text":"hi"`) || !strings.Contains(raw, `"sources":[{"url":"https://x.example/hi"`) {
		t.Errorf("call: %s", raw)
	}
	_, r, raw = call(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"txt":"hi"}}}`, nil)
	if r.Result["isError"] != true || !strings.Contains(raw, "bad args") {
		t.Errorf("strict args: %s", raw)
	}
	_, r, _ = call(t, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`, nil)
	if r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool: %+v", r)
	}
	_, r, _ = call(t, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`, nil)
	if r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("no tool name: %+v", r)
	}
	_, r, _ = call(t, h, `{"jsonrpc":"2.0","id":6,"method":"bogus"}`, nil)
	if r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Errorf("bogus: %+v", r)
	}
	if code, r, _ := call(t, h, `{"jsonrpc":"2.0","id":7,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "1999-01-01"}); code != http.StatusBadRequest || r.Error.Code != codeUnsupportedVersion {
		t.Errorf("bad header version: %d", code)
	}
	// A modern version in the header with no _meta in the body is a mismatch, not a legacy request.
	if code, r, _ := call(t, h, `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`, modernHeaders("tools/list", "")); code != http.StatusBadRequest || r.Error.Code != codeHeaderMismatch {
		t.Errorf("modern header without _meta: %d", code)
	}
}

func TestHandler_modernFlow(t *testing.T) {
	h := testHandler()
	_, r, _ := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`, modernHeaders("server/discover", ""))
	info, _ := r.Result["_meta"].(map[string]any)[metaServerInfo].(map[string]any)
	if r.Result["resultType"] != "complete" || r.Result["instructions"] != "hello" || info["name"] != "t" {
		t.Fatalf("discover: %+v", r)
	}
	_, r, _ = call(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{`+meta+`}}`, modernHeaders("tools/list", ""))
	if len(r.Result["tools"].([]any)) != 2 || r.Result["ttlMs"] != float64(time.Hour.Milliseconds()) || r.Result["cacheScope"] != "private" {
		t.Fatalf("list: %+v", r)
	}
	encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("echo")) + "?="
	_, _, raw := call(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{`+meta+`,"name":"echo","arguments":{"text":"x"}}}`, modernHeaders("tools/call", encoded))
	if !strings.Contains(raw, `"resultType":"complete"`) || !strings.Contains(raw, `"text":"x"`) {
		t.Errorf("call: %s", raw)
	}
	// -32602 on 200: a dual-era client reads any other 4xx as a legacy server.
	if code, r, _ := call(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{`+meta+`,"name":"nope"}}`, modernHeaders("tools/call", "nope")); code != http.StatusOK || r.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool: %d", code)
	}
	for name, hdr := range map[string]map[string]string{
		"name mismatch":   modernHeaders("tools/call", "big"),
		"no name header":  modernHeaders("tools/call", ""),
		"bad base64 name": modernHeaders("tools/call", "=?base64?!!?="),
		"method mismatch": modernHeaders("tools/list", ""),
		"no version":      {"Mcp-Method": "tools/call", "Mcp-Name": "echo"},
	} {
		if code, r, _ := call(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{`+meta+`,"name":"echo"}}`, hdr); code != http.StatusBadRequest || r.Error.Code != codeHeaderMismatch {
			t.Errorf("%s: %d", name, code)
		}
	}
	code, r, _ := call(t, h, `{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2030-01-01"}}}`, modernHeaders("tools/list", ""))
	if code != http.StatusBadRequest || r.Error.Code != codeUnsupportedVersion || r.Error.Data["requested"] != "2030-01-01" {
		t.Errorf("unsupported: %d %+v", code, r.Error)
	}
	for _, m := range []string{"resources/list", "initialize", "ping"} {
		if code, r, _ := call(t, h, `{"jsonrpc":"2.0","id":7,"method":"`+m+`","params":{`+meta+`}}`, modernHeaders(m, "")); code != http.StatusNotFound || r.Error.Code != codeMethodNotFound {
			t.Errorf("%s: %d", m, code)
		}
	}
	if code, _, _ := call(t, h, `{"jsonrpc":"2.0","method":"notifications/x","params":{`+meta+`}}`, modernHeaders("notifications/x", "")); code != http.StatusAccepted {
		t.Errorf("notification: %d", code)
	}
}

func TestHandler_annotationsAndTitles(t *testing.T) {
	_, r, _ := call(t, testHandler(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	tools := r.Result["tools"].([]any)
	echo, big := tools[0].(map[string]any), tools[1].(map[string]any)
	ea, ba := echo["annotations"].(map[string]any), big["annotations"].(map[string]any)
	if echo["title"] != "Echo" || ea["title"] != "Echo" || ea["readOnlyHint"] != true || ea["openWorldHint"] != false {
		t.Errorf("echo: %+v", echo)
	}
	if ba["readOnlyHint"] != false || ba["openWorldHint"] != true {
		t.Errorf("big: %+v", ba)
	}
}

func TestHandler_perRequestTextAliasesAndOptions(t *testing.T) {
	type k struct{}
	h := New(Server{
		Name:             "t",
		InstructionsFunc: func(ctx context.Context) string { return "built for " + ctx.Value(k{}).(string) },
		Tools: []Tool{{Name: "now", DescriptionFunc: func(ctx context.Context) string { return "desc for " + ctx.Value(k{}).(string) },
			Run: func(ctx context.Context, _ json.RawMessage) Result {
				dl, _ := ctx.Deadline()
				return Result{Text: strings.Repeat("y", 100), Sources: []Source{}, IsError: time.Until(dl) > 2*time.Second}
			}}},
		Aliases:      map[string]string{"old_now": "now"},
		ToolsTTL:     15 * time.Minute,
		ToolTimeout:  time.Second,
		MaxTextBytes: 50,
	})
	withK := func(body string, hdr map[string]string) (int, rpcReply, string) {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), k{}, "u1"))
		for name, v := range hdr {
			req.Header.Set(name, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var r rpcReply
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return rec.Code, r, rec.Body.String()
	}
	_, r, _ := withK(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`, nil)
	if r.Result["instructions"] != "built for u1" {
		t.Errorf("instructions: %v", r.Result["instructions"])
	}
	_, r, _ = withK(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta+`}}`, modernHeaders("tools/list", ""))
	if r.Result["tools"].([]any)[0].(map[string]any)["description"] != "desc for u1" || r.Result["ttlMs"] != float64(900000) {
		t.Errorf("list: %+v", r.Result)
	}
	_, r, raw := withK(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"old_now","arguments":null}}`, nil)
	if text := toolText(t, r); len(text) > 50 || !strings.HasSuffix(text, truncatedNote) {
		t.Errorf("alias call text %q", text)
	}
	if r.Result["isError"] == true {
		t.Error("ToolTimeout not applied")
	}
	if !strings.Contains(raw, `"structuredContent":{"sources":[]}`) {
		t.Errorf("empty sources must go out as an empty list: %s", raw)
	}
	_, _, raw = withK(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if strings.Contains(raw, "old_now") {
		t.Error("an alias is listed")
	}
}

func TestHandler_longTextIsCappedAtALine(t *testing.T) {
	_, r, _ := call(t, testHandler(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"big"}}`, nil)
	text := toolText(t, r)
	if len(text) > defaultMaxTextBytes || !strings.HasSuffix(text, "line of text"+truncatedNote) {
		t.Errorf("len %d, tail %q", len(text), text[len(text)-40:])
	}
	if CapText("short", 10) != "short" || CapText("much too long", 5) != "" {
		t.Error("capText edges")
	}
	if got := CapText(strings.Repeat("ä", 30), 30); len(got) > 30 || !strings.HasSuffix(got, truncatedNote) || !strings.HasPrefix(got, "ä") {
		t.Errorf("rune cut %q", got)
	}
}

func TestHandler_filesAsResourcesWithinTheCap(t *testing.T) {
	big := strings.Repeat("x", maxFilesBytes)
	h := New(Server{Name: "t", Tools: []Tool{{
		Name: "files",
		Run: func(context.Context, json.RawMessage) Result {
			return Result{Text: "here", Files: []File{
				{Name: "a.csv", MimeType: "text/csv", Text: "x,y\n1,2\n"},
				{Name: "big.csv", MimeType: "text/csv", Text: big},
				{Name: "b.json", MimeType: "application/json", Text: "{}"},
			}}
		},
	}}})
	_, r, raw := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"files"}}`, nil)
	content, _ := r.Result["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("want text + 2 resources (big.csv over the cap), got %d", len(content))
	}
	if text := toolText(t, r); !strings.Contains(text, "not attached, too large: big.csv") {
		t.Errorf("text does not name the dropped file: %q", text)
	}
	res := content[1].(map[string]any)
	inner, _ := res["resource"].(map[string]any)
	if res["type"] != "resource" || inner["uri"] != "file:///a.csv" || inner["mimeType"] != "text/csv" || inner["text"] != "x,y\n1,2\n" {
		t.Errorf("resource: %+v", res)
	}
	if !strings.Contains(raw, `"uri":"file:///b.json"`) {
		t.Errorf("a small file after the dropped one is missing: %.200s", raw)
	}
	if len(raw) > 4<<20 {
		t.Errorf("response %d bytes, over 4 MiB", len(raw))
	}
}

// A text at the cap still ends with the note on dropped files.
func TestHandler_droppedNoteSurvivesALongText(t *testing.T) {
	h := New(Server{Name: "t", Tools: []Tool{{
		Name: "long",
		Run: func(context.Context, json.RawMessage) Result {
			return Result{Text: strings.Repeat("a line of analysis\n", 3000), Files: []File{{Name: "big.csv", MimeType: "text/csv", Text: strings.Repeat("x", maxFilesBytes+1)}}}
		},
	}}})
	_, r, _ := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"long"}}`, nil)
	text := toolText(t, r)
	if !strings.HasSuffix(text, "(not attached, too large: big.csv)") || len(text) > defaultMaxTextBytes {
		t.Errorf("len %d, tail %q", len(text), text[len(text)-80:])
	}
}

func TestHandler_userFromHeader(t *testing.T) {
	who := Tool{Name: "who", Run: func(ctx context.Context, _ json.RawMessage) Result { return Result{Text: "user=" + User(ctx)} }}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"who"}}`
	h := New(Server{Name: "t", UserHeader: "X-User", Tools: []Tool{who}})
	if _, r, _ := call(t, h, body, map[string]string{"X-User": "  u-42 "}); toolText(t, r) != "user=u-42" {
		t.Fatalf("user from header: %q", toolText(t, r))
	}
	if _, r, _ := call(t, h, body, nil); toolText(t, r) != "user=" {
		t.Errorf("no header must mean no user: %q", toolText(t, r))
	}
	// Without a UserHeader no request header becomes the user.
	h = New(Server{Name: "t", Tools: []Tool{who}})
	if _, r, _ := call(t, h, body, map[string]string{"X-User": "u-42"}); toolText(t, r) != "user=" {
		t.Errorf("unconfigured header used: %q", toolText(t, r))
	}
}

func TestWithUser(t *testing.T) {
	if User(context.Background()) != "" {
		t.Error("empty context has a user")
	}
	if got := User(WithUser(context.Background(), "abc")); got != "abc" {
		t.Errorf("got %q", got)
	}
}

func TestOneLine(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"  a\n b\tc  ", 10, "a b c"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 4, "abc…"},
		{"ab cdef", 4, "ab…"}, // the space before the cut goes
		{"äöüéèà", 3, "äö…"},  // runes, not bytes
		{"abc", 1, "…"},
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"", 0, ""},
	}
	for _, tc := range tests {
		if got := OneLine(tc.s, tc.n); got != tc.want {
			t.Errorf("OneLine(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
