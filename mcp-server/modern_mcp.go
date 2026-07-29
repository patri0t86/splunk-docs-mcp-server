package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const modernProtocolVersion = "2026-07-28"

type modernMCPHandler struct {
	allowedOrigins map[string]struct{}
}

type dualMCPHandler struct {
	modern         http.Handler
	legacy         http.Handler
	allowedOrigins map[string]struct{}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func newModernMCPHandler(origins string) http.Handler {
	return &modernMCPHandler{allowedOrigins: parseAllowedOrigins(origins)}
}

func newDualMCPHandler(modern, legacy http.Handler, origins string) http.Handler {
	return &dualMCPHandler{
		modern:         modern,
		legacy:         legacy,
		allowedOrigins: parseAllowedOrigins(origins),
	}
}

func newLegacyMCPHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "splunk-docs", Version: "1.2.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_docs",
		Description: "Search the indexed Splunk documentation corpus. Optionally narrow results by product and version. Use get_page with a result URL for the full page.",
	}, searchDocs)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_page",
		Description: "Retrieve the complete indexed content for a Splunk documentation URL.",
	}, getPage)
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{},
	)
}

func parseAllowedOrigins(origins string) map[string]struct{} {
	allowedOrigins := make(map[string]struct{})
	for _, origin := range strings.Split(origins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowedOrigins[origin] = struct{}{}
		}
	}
	return allowedOrigins
}

func originAllowed(r *http.Request, allowedOrigins map[string]struct{}) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	_, ok := allowedOrigins[origin]
	return ok
}

func (h *dualMCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r, h.allowedOrigins) {
		http.Error(w, "Forbidden: invalid Origin header", http.StatusForbidden)
		return
	}
	if r.Header.Get("MCP-Protocol-Version") == modernProtocolVersion {
		h.modern.ServeHTTP(w, r)
		return
	}
	h.legacy.ServeHTTP(w, r)
}

func (h *modernMCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !originAllowed(r, h.allowedOrigins) {
		http.Error(w, "Forbidden: invalid Origin header", http.StatusForbidden)
		return
	}
	if !acceptsMCPResponse(r.Header.Values("Accept")) {
		h.rpcError(w, nil, http.StatusBadRequest, -32020, "Header mismatch: Accept must include application/json and text/event-stream", nil)
		return
	}
	if !strings.EqualFold(strings.Split(r.Header.Get("Content-Type"), ";")[0], "application/json") {
		h.rpcError(w, nil, http.StatusUnsupportedMediaType, -32020, "Content-Type must be application/json", nil)
		return
	}

	var request rpcRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil || decoder.More() || request.JSONRPC != "2.0" || request.Method == "" || len(request.ID) == 0 || string(request.ID) == "null" {
		h.rpcError(w, nil, http.StatusBadRequest, -32600, "Invalid Request", nil)
		return
	}
	if err := validateModernRequest(r, request); err != nil {
		var protocolError *protocolValidationError
		if errors.As(err, &protocolError) {
			h.rpcError(w, request.ID, http.StatusBadRequest, protocolError.Code, protocolError.Message, protocolError.Data)
			return
		}
		h.rpcError(w, request.ID, http.StatusBadRequest, -32020, "Header mismatch: "+err.Error(), nil)
		return
	}

	switch request.Method {
	case "server/discover":
		h.rpcResult(w, request.ID, map[string]any{
			"supportedVersions": []string{modernProtocolVersion},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      "Use search_docs to find relevant Splunk documentation, then get_page to retrieve a complete result page.",
		})
	case "tools/list":
		h.rpcResult(w, request.ID, map[string]any{"tools": modernTools()})
	case "tools/call":
		h.callTool(w, r, request)
	default:
		h.rpcError(w, request.ID, http.StatusNotFound, -32601, "Method not found: "+request.Method, nil)
	}
}

func acceptsMCPResponse(values []string) bool {
	acceptsJSON, acceptsSSE := false, false
	for _, value := range values {
		for _, mediaType := range strings.Split(value, ",") {
			mediaType = strings.TrimSpace(strings.Split(mediaType, ";")[0])
			acceptsJSON = acceptsJSON || mediaType == "application/json" || mediaType == "*/*"
			acceptsSSE = acceptsSSE || mediaType == "text/event-stream" || mediaType == "*/*"
		}
	}
	return acceptsJSON && acceptsSSE
}

func validateModernRequest(r *http.Request, request rpcRequest) error {
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return &protocolValidationError{Code: -32602, Message: "Invalid params: request params must be an object"}
	}
	var meta struct {
		ProtocolVersion    string          `json:"io.modelcontextprotocol/protocolVersion"`
		ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
	}
	if err := json.Unmarshal(params["_meta"], &meta); err != nil || meta.ProtocolVersion == "" || len(meta.ClientCapabilities) == 0 || string(meta.ClientCapabilities) == "null" {
		return &protocolValidationError{Code: -32602, Message: "Invalid params: request _meta is missing required protocol metadata"}
	}
	if r.Header.Get("MCP-Protocol-Version") != meta.ProtocolVersion {
		return fmt.Errorf("MCP-Protocol-Version does not match request metadata")
	}
	if meta.ProtocolVersion != modernProtocolVersion {
		return &protocolValidationError{
			Code:    -32022,
			Message: "Unsupported protocol version",
			Data:    map[string]any{"supported": []string{modernProtocolVersion}, "requested": meta.ProtocolVersion},
		}
	}
	if r.Header.Get("Mcp-Method") != request.Method {
		return fmt.Errorf("Mcp-Method does not match request method")
	}
	if request.Method == "tools/call" {
		var name string
		if err := json.Unmarshal(params["name"], &name); err != nil || name == "" {
			return fmt.Errorf("tools/call requires a name")
		}
		if headerName, err := decodeMCPHeader(r.Header.Get("Mcp-Name")); err != nil || headerName != name {
			return fmt.Errorf("Mcp-Name does not match tool name")
		}
	}
	return nil
}

type protocolValidationError struct {
	Code    int
	Message string
	Data    any
}

func (e *protocolValidationError) Error() string { return e.Message }

func decodeMCPHeader(value string) (string, error) {
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, "=?base64?"), "?="))
		return string(decoded), err
	}
	return value, nil
}

func modernTools() []map[string]any {
	return []map[string]any{
		{
			"name": "search_docs", "title": "Search Splunk documentation",
			"description": "Search the indexed Splunk documentation corpus. Optionally narrow results by product and version. Use get_page with a result URL for the full page.",
			"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "Search query."},
				"product":     map[string]any{"type": "string", "description": "Optional indexed product filter."},
				"version":     map[string]any{"type": "string", "description": "Optional document version filter."},
				"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": maxResultsCap, "description": "Maximum results to return; default 5."},
			}, "required": []string{"query"}},
		},
		{
			"name": "get_page", "title": "Get Splunk documentation page",
			"description": "Retrieve the complete indexed content for a Splunk documentation URL.",
			"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"url": map[string]any{"type": "string", "format": "uri", "description": "Documentation URL returned by search_docs."},
			}, "required": []string{"url"}},
		},
	}
}

func (h *modernMCPHandler) callTool(w http.ResponseWriter, r *http.Request, request rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		h.rpcError(w, request.ID, http.StatusBadRequest, -32602, "Invalid params", nil)
		return
	}
	var result *mcp.CallToolResult
	var err error
	switch params.Name {
	case "search_docs":
		var arguments SearchArgs
		if err = json.Unmarshal(params.Arguments, &arguments); err == nil {
			result, _, err = searchDocs(r.Context(), nil, arguments)
		}
	case "get_page":
		var arguments GetPageArgs
		if err = json.Unmarshal(params.Arguments, &arguments); err == nil {
			result, _, err = getPage(r.Context(), nil, arguments)
		}
	default:
		h.rpcError(w, request.ID, http.StatusBadRequest, -32602, "Unknown tool: "+params.Name, nil)
		return
	}
	if err != nil {
		h.rpcToolError(w, request.ID, err.Error())
		return
	}
	texts := make([]map[string]string, 0, len(result.Content))
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, map[string]string{"type": "text", "text": text.Text})
		}
	}
	h.rpcResult(w, request.ID, map[string]any{"content": texts})
}

func (h *modernMCPHandler) rpcToolError(w http.ResponseWriter, id json.RawMessage, message string) {
	h.rpcResult(w, id, map[string]any{"content": []map[string]string{{"type": "text", "text": message}}, "isError": true})
}

func (h *modernMCPHandler) rpcResult(w http.ResponseWriter, id json.RawMessage, result map[string]any) {
	result["resultType"] = "complete"
	result["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "splunk-docs", "version": "1.2.0"}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (h *modernMCPHandler) rpcError(w http.ResponseWriter, id json.RawMessage, status, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": rpcError{Code: code, Message: message, Data: data}})
}
