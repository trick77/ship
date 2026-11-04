package mcpserver

import (
	"encoding/json"
	"net/http"
)

// JSON-RPC and MCP error codes this server emits.
const (
	codeParseError         = -32700
	codeInvalidRequest     = -32600
	codeMethodNotFound     = -32601
	codeInvalidParams      = -32602
	codeHeaderMismatch     = -32020
	codeUnsupportedVersion = -32022
)

// request is one JSON-RPC message. ID is kept raw so it is echoed exactly; a missing id makes the message a
// notification.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r request) isNotification() bool { return len(r.ID) == 0 }

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// nullID stands in for the id of a message that could not be read.
var nullID = json.RawMessage("null")

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeMessage(w, http.StatusOK, response{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	if len(id) == 0 {
		id = nullID
	}
	writeMessage(w, status, response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

func writeMessage(w http.ResponseWriter, status int, msg response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(msg)
}
