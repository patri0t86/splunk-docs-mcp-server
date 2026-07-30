package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestModernMCPDiscover(t *testing.T) {
	handler := newModernMCPHandler("")
	rec := serveModernRequest(t, handler, "server/discover", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var response struct {
		Result struct {
			ResultType        string   `json:"resultType"`
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"result"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Result.ResultType != "complete" {
		t.Errorf("resultType = %q, want complete", response.Result.ResultType)
	}
	if len(response.Result.SupportedVersions) != 1 || response.Result.SupportedVersions[0] != modernProtocolVersion {
		t.Errorf("supportedVersions = %v, want [%s]", response.Result.SupportedVersions, modernProtocolVersion)
	}
}

func TestModernMCPToolsList(t *testing.T) {
	handler := newModernMCPHandler("")
	rec := serveModernRequest(t, handler, "tools/list", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	want := []string{"search_docs", "get_section", "get_page", "list_docsets", "compare_versions"}
	var got []string
	for _, tool := range response.Result.Tools {
		got = append(got, tool.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	for _, tool := range response.Result.Tools {
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s input schema type = %v, want object", tool.Name, tool.InputSchema["type"])
		}
		// Both transports must describe a tool identically, otherwise a client
		// gets different guidance depending on which protocol it negotiated.
		if tool.Description != toolDescriptions[tool.Name] {
			t.Errorf("%s description does not match the shared description", tool.Name)
		}
	}
	if len(toolDescriptions) != len(want) {
		t.Errorf("toolDescriptions has %d entries, want %d", len(toolDescriptions), len(want))
	}
}

func TestModernMCPRejectsInvalidOriginAndHeaders(t *testing.T) {
	handler := newModernMCPHandler("https://inspector.example")
	request := modernRequest(t, "server/discover", map[string]any{})
	request.Header.Set("Origin", "https://untrusted.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusForbidden {
		t.Errorf("invalid Origin status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	request = modernRequest(t, "server/discover", map[string]any{})
	request.Header.Set("Mcp-Method", "tools/list")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mismatched Mcp-Method status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var response struct {
		Error rpcError `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != -32020 {
		t.Errorf("error code = %d, want -32020", response.Error.Code)
	}
}

func TestModernMCPAllowsOnlyPost(t *testing.T) {
	handler := newModernMCPHandler("")
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}
}

func TestDualMCPHandlerRoutesByProtocolEra(t *testing.T) {
	modern := markerHandler("modern")
	legacy := markerHandler("legacy")
	handler := newDualMCPHandler(modern, legacy, "https://inspector.example")

	modernRequest := modernRequest(t, "server/discover", map[string]any{})
	modernRec := httptest.NewRecorder()
	handler.ServeHTTP(modernRec, modernRequest)
	if got := modernRec.Body.String(); got != "modern" {
		t.Errorf("modern request routed to %q, want modern", got)
	}

	legacyRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	legacyRequest.Header.Set("MCP-Protocol-Version", "2025-11-25")
	legacyRec := httptest.NewRecorder()
	handler.ServeHTTP(legacyRec, legacyRequest)
	if got := legacyRec.Body.String(); got != "legacy" {
		t.Errorf("legacy request routed to %q, want legacy", got)
	}

	legacyRequest = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	legacyRequest.Header.Set("Origin", "https://untrusted.example")
	legacyRec = httptest.NewRecorder()
	handler.ServeHTTP(legacyRec, legacyRequest)
	if legacyRec.Code != http.StatusForbidden {
		t.Errorf("legacy invalid Origin status = %d, want %d", legacyRec.Code, http.StatusForbidden)
	}
}

func TestDualMCPHandlerAcceptsLegacyInitialize(t *testing.T) {
	handler := newDualMCPHandler(newModernMCPHandler(""), newLegacyMCPHandler(), "")
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]string{"name": "legacy-test", "version": "1.0.0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy initialize status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Header().Get("Mcp-Session-Id") == "" {
		t.Error("legacy initialize response is missing Mcp-Session-Id")
	}
}

func TestModernMCPProtocolErrors(t *testing.T) {
	handler := newModernMCPHandler("")
	for _, testCase := range []struct {
		name    string
		version string
		meta    map[string]any
		want    int
	}{
		{
			name:    "missing client capabilities",
			version: modernProtocolVersion,
			meta:    map[string]any{"io.modelcontextprotocol/protocolVersion": modernProtocolVersion},
			want:    -32602,
		},
		{
			name:    "unsupported protocol version",
			version: "2025-11-25",
			meta: map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2025-11-25",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
			want: -32022,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "server/discover",
				"params": map[string]any{"_meta": testCase.meta},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("MCP-Protocol-Version", testCase.version)
			req.Header.Set("Mcp-Method", "server/discover")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			var response struct {
				Error rpcError `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusBadRequest || response.Error.Code != testCase.want {
				t.Errorf("status/code = %d/%d, want %d/%d", rec.Code, response.Error.Code, http.StatusBadRequest, testCase.want)
			}
		})
	}
}

func serveModernRequest(t *testing.T, handler http.Handler, method string, params map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, modernRequest(t, method, params))
	return rec
}

func modernRequest(t *testing.T, method string, params map[string]any) *http.Request {
	t.Helper()
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    modernProtocolVersion,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", modernProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	return req
}

func markerHandler(marker string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(marker))
	})
}
