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

const serverVersion = "1.3.0"

// serverInstructions tells a client how the tools compose: discovery, then
// bounded citable evidence, with the whole page reserved for broad reading.
const serverInstructions = "Use search_docs to find relevant Splunk documentation sections, then get_section with a result's section_id for bounded, citable evidence, or get_page for a complete page. " +
	"Call list_docsets to discover the products, versions and manuals that are indexed before filtering on them, and compare_versions to see what changed between two versions of the same page."

// toolDescriptions is shared by both protocol handlers so the two transports
// can never advertise different tools.
var toolDescriptions = map[string]string{
	"search_docs": "Search the indexed Splunk documentation corpus and return ranked sections with stable section ids, citation URLs and match provenance. " +
		"Copies of the same page across product versions are collapsed to the newest by default. " +
		"Follow up with get_section for the exact text, or get_page for the whole page.",
	"get_section": "Retrieve one documentation section by the section_id from a search_docs result, optionally with neighbouring sections for context. " +
		"Prefer this over get_page when you need quotable evidence rather than a whole page.",
	"get_page":     "Retrieve the complete indexed content for a Splunk documentation URL. Use when a whole page is genuinely needed; get_section is cheaper and cites more precisely.",
	"list_docsets": "List the indexed products, their versions, page counts, manuals and index freshness. Call this to discover valid product and version filter values.",
	"compare_versions": "Report the sections that were added, removed or changed between two versions of the same documentation page, with unified text diffs. " +
		"Differences come from the indexed source text, not from a model-written summary.",
}

func ptr[T any](v T) *T { return &v }

// Every tool is a read-only lookup against a local index: no writes, no
// side effects, and no requests to anything outside this deployment.
func readOnlyAnnotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: ptr(false),
		IdempotentHint:  true,
		OpenWorldHint:   ptr(false),
	}
}

func newLegacyMCPHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "splunk-docs", Version: serverVersion}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_docs",
		Title:       "Search Splunk documentation",
		Description: toolDescriptions["search_docs"],
		Annotations: readOnlyAnnotations(),
	}, searchDocs)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_section",
		Title:       "Get a documentation section",
		Description: toolDescriptions["get_section"],
		Annotations: readOnlyAnnotations(),
	}, getSection)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_page",
		Title:       "Get Splunk documentation page",
		Description: toolDescriptions["get_page"],
		Annotations: readOnlyAnnotations(),
	}, getPage)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_docsets",
		Title:       "List indexed docsets",
		Description: toolDescriptions["list_docsets"],
		Annotations: readOnlyAnnotations(),
	}, listDocsets)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "compare_versions",
		Title:       "Compare two versions of a page",
		Description: toolDescriptions["compare_versions"],
		Annotations: readOnlyAnnotations(),
	}, compareVersions)
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
			"instructions":      serverInstructions,
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
	stringArray := func(description string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": description}
	}
	return []map[string]any{
		{
			"name": "search_docs", "title": "Search Splunk documentation",
			"description": toolDescriptions["search_docs"],
			"annotations": readOnlyToolAnnotations,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "Search query."},
				"product":     map[string]any{"type": "string", "description": "Optional single indexed product filter."},
				"products":    stringArray("Optional list of indexed products to restrict the search to."),
				"version":     map[string]any{"type": "string", "description": "Optional single document version filter."},
				"versions":    stringArray("Optional list of document versions to restrict the search to."),
				"manuals":     stringArray("Optional list of manuals to restrict the search to."),
				"mode":        map[string]any{"type": "string", "enum": []string{modeAuto, modeExact, modeConceptual}, "description": "Retrieval mode; auto detects technical identifiers."},
				"latest_only": map[string]any{"type": "boolean", "description": "Collapse copies of a page across versions to the newest; default true."},
				"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": maxResultsCap, "description": "Maximum results to return; default 5."},
			}, "required": []string{"query"}},
			"outputSchema": searchOutputSchema(),
		},
		{
			"name": "get_section", "title": "Get a documentation section",
			"description": toolDescriptions["get_section"],
			"annotations": readOnlyToolAnnotations,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"section_id":     map[string]any{"type": "string", "description": "Section identifier from a search_docs result."},
				"context_before": map[string]any{"type": "integer", "minimum": 0, "maximum": maxContextSections, "description": "Preceding sections to include; default 0."},
				"context_after":  map[string]any{"type": "integer", "minimum": 0, "maximum": maxContextSections, "description": "Following sections to include; default 0."},
				"max_chars":      map[string]any{"type": "integer", "minimum": 1, "maximum": maxSectionChars, "description": "Truncate returned text; default 12000."},
			}, "required": []string{"section_id"}},
		},
		{
			"name": "get_page", "title": "Get Splunk documentation page",
			"description": toolDescriptions["get_page"],
			"annotations": readOnlyToolAnnotations,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"url": map[string]any{"type": "string", "format": "uri", "description": "Documentation URL returned by search_docs."},
			}, "required": []string{"url"}},
		},
		{
			"name": "list_docsets", "title": "List indexed docsets",
			"description": toolDescriptions["list_docsets"],
			"annotations": readOnlyToolAnnotations,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"product":         map[string]any{"type": "string", "description": "Optional product to describe in detail."},
				"include_manuals": map[string]any{"type": "boolean", "description": "Also list manuals; only applied together with product."},
			}},
		},
		{
			"name": "compare_versions", "title": "Compare two versions of a page",
			"description": toolDescriptions["compare_versions"],
			"annotations": readOnlyToolAnnotations,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"document_id":  map[string]any{"type": "string", "description": "Version-independent page id from a search_docs result."},
				"url":          map[string]any{"type": "string", "format": "uri", "description": "Any indexed URL of the page, if document_id is unknown."},
				"from_version": map[string]any{"type": "string", "description": "Older version to compare."},
				"to_version":   map[string]any{"type": "string", "description": "Newer version to compare."},
				"max_chars":    map[string]any{"type": "integer", "minimum": 1, "maximum": maxSectionChars, "description": "Truncate the rendered diff; default 12000."},
			}, "required": []string{"from_version", "to_version"}},
		},
	}
}

var readOnlyToolAnnotations = map[string]any{
	"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false,
}

// searchOutputSchema describes the structured search payload. Search is the
// tool whose results get carried around and cited, so its shape is declared
// rather than left for the client to infer from the text rendering.
func searchOutputSchema() map[string]any {
	stringProp := map[string]any{"type": "string"}
	stringArrayProp := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": stringProp,
			"mode":  stringProp,
			"count": map[string]any{"type": "integer"},
			"results": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"rank":              map[string]any{"type": "integer"},
						"section_id":        stringProp,
						"document_id":       stringProp,
						"title":             stringProp,
						"product":           stringProp,
						"version":           stringProp,
						"manual":            stringProp,
						"breadcrumb":        stringArrayProp,
						"heading":           stringProp,
						"url":               stringProp,
						"anchor":            stringProp,
						"citation_url":      stringProp,
						"snippet":           stringProp,
						"match_types":       stringArrayProp,
						"content_hash":      stringProp,
						"source_updated_at": stringProp,
					},
					"required": []string{"rank", "section_id", "document_id", "title", "product", "url", "citation_url", "snippet", "match_types"},
				},
			},
		},
		"required": []string{"query", "mode", "count", "results"},
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
	var structured any
	var err error
	switch params.Name {
	case "search_docs":
		var arguments SearchArgs
		if err = json.Unmarshal(params.Arguments, &arguments); err == nil {
			var out *SearchOutput
			result, out, err = searchDocs(r.Context(), nil, arguments)
			structured = nilIfEmpty(out)
		}
	case "get_section":
		var arguments GetSectionArgs
		if err = json.Unmarshal(params.Arguments, &arguments); err == nil {
			var out *SectionOutput
			result, out, err = getSection(r.Context(), nil, arguments)
			structured = nilIfEmpty(out)
		}
	case "get_page":
		var arguments GetPageArgs
		if err = json.Unmarshal(params.Arguments, &arguments); err == nil {
			var out *PageOutput
			result, out, err = getPage(r.Context(), nil, arguments)
			structured = nilIfEmpty(out)
		}
	case "list_docsets":
		var arguments ListDocsetsArgs
		if err = json.Unmarshal(params.Arguments, &arguments); err == nil {
			var out *ListDocsetsOutput
			result, out, err = listDocsets(r.Context(), nil, arguments)
			structured = nilIfEmpty(out)
		}
	case "compare_versions":
		var arguments CompareVersionsArgs
		if err = json.Unmarshal(params.Arguments, &arguments); err == nil {
			var out *CompareVersionsOutput
			result, out, err = compareVersions(r.Context(), nil, arguments)
			structured = nilIfEmpty(out)
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
	payload := map[string]any{"content": texts}
	if structured != nil {
		payload["structuredContent"] = structured
	}
	h.rpcResult(w, request.ID, payload)
}

// nilIfEmpty converts a typed nil pointer to an untyped nil, so a tool that
// returned no structured payload doesn't serialize as "structuredContent": null.
func nilIfEmpty[T any](out *T) any {
	if out == nil {
		return nil
	}
	return out
}

func (h *modernMCPHandler) rpcToolError(w http.ResponseWriter, id json.RawMessage, message string) {
	h.rpcResult(w, id, map[string]any{"content": []map[string]string{{"type": "text", "text": message}}, "isError": true})
}

func (h *modernMCPHandler) rpcResult(w http.ResponseWriter, id json.RawMessage, result map[string]any) {
	result["resultType"] = "complete"
	result["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "splunk-docs", "version": serverVersion}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (h *modernMCPHandler) rpcError(w http.ResponseWriter, id json.RawMessage, status, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": rpcError{Code: code, Message: message, Data: data}})
}
