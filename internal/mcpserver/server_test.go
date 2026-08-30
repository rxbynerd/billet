package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/cost"
)

func newTestServer(t *testing.T, budget *cost.Guard) *httptest.Server {
	t.Helper()
	if budget == nil {
		budget = cost.NewGuard(0, &bytes.Buffer{})
	}
	s := New(backend.NewMemoryBackend(), budget)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestSaveThenSearchViaMCPClient drives the server through the reference
// MCP client (full initialize handshake, streamable transport) to prove
// the tool surface works end to end for a spec-compliant caller.
func TestSaveThenSearchViaMCPClient(t *testing.T) {
	ts := newTestServer(t, nil)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	saveResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": "the paddock gate was left open", "kind": "fact"},
	})
	if err != nil {
		t.Fatalf("CallTool save_memory: %v", err)
	}
	if saveResult.IsError {
		t.Fatalf("save_memory returned a tool error: %+v", saveResult.Content)
	}
	var saved saveMemoryOutput
	if err := decodeStructured(saveResult, &saved); err != nil {
		t.Fatalf("decode save_memory result: %v", err)
	}
	if saved.MemoryID == "" || !saved.Accepted {
		t.Fatalf("save_memory result = %+v, want a memory_id and accepted=true", saved)
	}

	searchResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_memory",
		Arguments: map[string]any{"query": "paddock gate"},
	})
	if err != nil {
		t.Fatalf("CallTool search_memory: %v", err)
	}
	if searchResult.IsError {
		t.Fatalf("search_memory returned a tool error: %+v", searchResult.Content)
	}
	var searched searchMemoryOutput
	if err := decodeStructured(searchResult, &searched); err != nil {
		t.Fatalf("decode search_memory result: %v", err)
	}
	if len(searched.Records) != 1 || searched.Records[0].MemoryID != saved.MemoryID {
		t.Fatalf("search_memory result = %+v, want the saved record first", searched)
	}
}

// TestSaveMemoryValidatesKind exercises the tool-error path for an
// invalid kind hint via the reference client.
func TestSaveMemoryValidatesKind(t *testing.T) {
	ts := newTestServer(t, nil)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": "x", "kind": "opinion"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatal("save_memory with an invalid kind did not report a tool error")
	}
}

// TestBudgetExceededRejectsToolCalls pins the per-call cost governance
// rejection path: once the rolling estimate reaches the cap, further
// save_memory/search_memory calls fail with a tool error, not a silent
// pass-through.
func TestBudgetExceededRejectsToolCalls(t *testing.T) {
	budget := cost.NewGuard(cost.EstimatedCostPerCallGBP, &bytes.Buffer{})
	ts := newTestServer(t, budget)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	// First call consumes the entire (tiny) budget.
	first, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": "first call"},
	})
	if err != nil {
		t.Fatalf("CallTool (first): %v", err)
	}
	if first.IsError {
		t.Fatalf("first call unexpectedly rejected: %+v", first.Content)
	}

	// Second call must be rejected as a tool error mentioning budget.
	second, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": "second call"},
	})
	if err != nil {
		t.Fatalf("CallTool (second): %v", err)
	}
	if !second.IsError {
		t.Fatal("second call over budget was not rejected")
	}
	if !containsText(second, "budget") {
		t.Errorf("rejection message does not mention budget: %+v", second.Content)
	}

	// search_memory is gated by the same guard.
	searchResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_memory",
		Arguments: map[string]any{"query": "anything"},
	})
	if err != nil {
		t.Fatalf("CallTool search_memory: %v", err)
	}
	if !searchResult.IsError {
		t.Fatal("search_memory over budget was not rejected")
	}
}

// TestStirrupCompatibleRawJSONRPC drives the server with a minimal,
// hand-rolled JSON-RPC client shaped exactly like Stirrup's
// (harness/internal/mcp/client.go): no Accept header, no "initialize"
// call, and a plain (non-SSE) JSON response body expected. This pins the
// interop fix documented in docs/DECISIONS.md.
func TestStirrupCompatibleRawJSONRPC(t *testing.T) {
	ts := newTestServer(t, nil)

	call := func(t *testing.T, method string, params any) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  method,
			"params":  params,
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		// Deliberately no Accept header and no prior "initialize" call.

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d", method, resp.StatusCode)
		}

		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s: decode plain JSON body (not SSE-framed): %v", method, err)
		}
		return out
	}

	toolsList := call(t, "tools/list", struct{}{})
	if _, ok := toolsList["result"]; !ok {
		t.Fatalf("tools/list response has no result: %+v", toolsList)
	}

	saveResp := call(t, "tools/call", map[string]any{
		"name":      "save_memory",
		"arguments": json.RawMessage(`{"content":"hay delivered on schedule"}`),
	})
	if _, ok := saveResp["error"]; ok {
		t.Fatalf("save_memory JSON-RPC error: %+v", saveResp)
	}
}

func decodeStructured(result *mcp.CallToolResult, out any) error {
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func containsText(result *mcp.CallToolResult, substr string) bool {
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(strings.ToLower(tc.Text), substr) {
			return true
		}
	}
	return false
}
