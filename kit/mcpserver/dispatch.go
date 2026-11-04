package mcpserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// ModernProtocolVersion is the stateless protocol revision: no initialize, no session, every request
// self-describing through params._meta and headers.
const ModernProtocolVersion = "2026-07-28"

// legacyVersions are the initialize-era revisions still answered.
var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// defaultLegacyVersion is offered to an initialize asking for a version this server does not know.
const defaultLegacyVersion = "2025-06-18"

const (
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaServerInfo      = "io.modelcontextprotocol/serverInfo"
)

// envelope is the part of params every method shares.
type envelope struct {
	Meta map[string]json.RawMessage `json:"_meta"`
	Name string                     `json:"name"`
}

func (h *handler) dispatch(w http.ResponseWriter, r *http.Request, req request) {
	var env envelope
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &env); err != nil {
			writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "params must be an object", nil)
			return
		}
	}
	var version string
	if raw, ok := env.Meta[metaProtocolVersion]; ok {
		_ = json.Unmarshal(raw, &version)
	}
	if version != "" {
		h.dispatchModern(w, r, req, env, version)
		return
	}
	h.dispatchLegacy(w, r, req)
}

func (h *handler) dispatchModern(w http.ResponseWriter, r *http.Request, req request, env envelope, version string) {
	if version != ModernProtocolVersion {
		writeRPCError(w, http.StatusBadRequest, req.ID, codeUnsupportedVersion, "Unsupported protocol version",
			map[string]any{"supported": []string{ModernProtocolVersion}, "requested": version})
		return
	}
	if r.Header.Get("MCP-Protocol-Version") != version {
		writeRPCError(w, http.StatusBadRequest, req.ID, codeHeaderMismatch, "MCP-Protocol-Version header does not match _meta", nil)
		return
	}
	if r.Header.Get("Mcp-Method") != req.Method {
		writeRPCError(w, http.StatusBadRequest, req.ID, codeHeaderMismatch, "Mcp-Method header does not match the method", nil)
		return
	}
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == "tools/call" && decodeHeaderValue(r.Header.Get("Mcp-Name")) != env.Name {
		writeRPCError(w, http.StatusBadRequest, req.ID, codeHeaderMismatch, "Mcp-Name header does not match params.name", nil)
		return
	}
	switch req.Method {
	case "server/discover":
		writeResult(w, req.ID, h.modern(map[string]any{
			"supportedVersions": []string{ModernProtocolVersion},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      h.instructions(r.Context()),
			"ttlMs":             h.ttlMs,
			"cacheScope":        "private",
		}))
	case "tools/list":
		writeResult(w, req.ID, h.modern(map[string]any{
			"tools":      h.toolDefs(r.Context()),
			"ttlMs":      h.ttlMs,
			"cacheScope": "private",
		}))
	case "tools/call":
		h.callTool(w, r, req, true)
	default:
		writeRPCError(w, http.StatusNotFound, req.ID, codeMethodNotFound, "method not found", nil)
	}
}

func (h *handler) dispatchLegacy(w http.ResponseWriter, r *http.Request, req request) {
	switch hv := r.Header.Get("MCP-Protocol-Version"); {
	case hv == ModernProtocolVersion:
		writeRPCError(w, http.StatusBadRequest, req.ID, codeHeaderMismatch, "a 2026-07-28 request must carry _meta", nil)
		return
	case hv != "" && !slices.Contains(legacyVersions, hv):
		writeRPCError(w, http.StatusBadRequest, req.ID, codeUnsupportedVersion, "Unsupported protocol version",
			map[string]any{"supported": append([]string{ModernProtocolVersion}, legacyVersions...), "requested": hv})
		return
	}
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := defaultLegacyVersion
		if slices.Contains(legacyVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		// Stateless: no Mcp-Session-Id, so every later request stands alone.
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      h.serverInfo(),
			"instructions":    h.instructions(r.Context()),
		})
	case "ping":
		writeResult(w, req.ID, map[string]any{})
	case "tools/list":
		writeResult(w, req.ID, map[string]any{"tools": h.toolDefs(r.Context())})
	case "tools/call":
		h.callTool(w, r, req, false)
	default:
		writeRPCError(w, http.StatusOK, req.ID, codeMethodNotFound, "method not found", nil)
	}
}

func (h *handler) serverInfo() map[string]string {
	return map[string]string{"name": h.s.Name, "version": h.s.Version}
}

// modern adds the fields every 2026-07-28 result carries.
func (h *handler) modern(result map[string]any) map[string]any {
	result["resultType"] = "complete"
	result["_meta"] = map[string]any{metaServerInfo: h.serverInfo()}
	return result
}

// decodeHeaderValue unwraps the =?base64?…?= form a client uses for a value that is not plain ASCII. Anything
// else is returned as is.
func decodeHeaderValue(v string) string {
	inner, ok := strings.CutPrefix(v, "=?base64?")
	if !ok {
		return v
	}
	inner, ok = strings.CutSuffix(inner, "?=")
	if !ok {
		return v
	}
	b, err := base64.StdEncoding.DecodeString(inner)
	if err != nil {
		return v
	}
	return string(b)
}
