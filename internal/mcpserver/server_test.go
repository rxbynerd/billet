package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/cost"
	"github.com/rxbynerd/billet/internal/service"
)

// erroringBackend always fails, so tests can assert on how a backend
// error is surfaced to an MCP caller without needing a real failing
// backend (AWS credentials, network, and so on).
type erroringBackend struct {
	err error
}

func (e erroringBackend) Save(context.Context, backend.SaveRequest) (string, error) {
	return "", e.err
}

func (e erroringBackend) Search(context.Context, backend.SearchRequest) ([]backend.Record, error) {
	return nil, e.err
}

// recordingBackend records the last SearchRequest it received, so tests
// can assert on what the MCP layer forwards to a Backend without needing
// a real one.
type recordingBackend struct {
	mu         sync.Mutex
	lastSearch backend.SearchRequest
}

func (r *recordingBackend) Save(context.Context, backend.SaveRequest) (string, error) {
	return "rec-id", nil
}

func (r *recordingBackend) Search(_ context.Context, req backend.SearchRequest) ([]backend.Record, error) {
	r.mu.Lock()
	r.lastSearch = req
	r.mu.Unlock()
	return nil, nil
}

func (r *recordingBackend) lastLimit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSearch.Limit
}

func newTestServer(t *testing.T, budget *cost.Guard) *httptest.Server {
	t.Helper()
	if budget == nil {
		budget = cost.NewGuard(0, &bytes.Buffer{})
	}
	s := New(service.New(backend.NewMemoryBackend(), budget))
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

// TestSaveMemoryBackendErrorReturnsGenericMessage pins the fix that keeps
// internal detail (here, an AWS account ID and ARN, as a real
// AccessDeniedException would carry) out of the tool result an
// unauthenticated MCP caller sees.
func TestSaveMemoryBackendErrorReturnsGenericMessage(t *testing.T) {
	const internalDetail = "AccessDeniedException: arn:aws:iam::123456789012:role/billet is not authorized (RequestId: abc-123)"
	s := New(service.New(erroringBackend{err: errors.New(internalDetail)}, cost.NewGuard(0, &bytes.Buffer{})))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": "x"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatal("save_memory did not report a tool error for a failing backend")
	}
	if containsText(result, "123456789012") || containsText(result, "arn:aws") {
		t.Fatalf("backend error detail leaked to the caller: %+v", result.Content)
	}
	if !containsText(result, "backend unavailable") {
		t.Errorf("save_memory error does not mention backend unavailable: %+v", result.Content)
	}
}

func TestSearchMemoryBackendErrorReturnsGenericMessage(t *testing.T) {
	const internalDetail = "ThrottlingException: rate exceeded for request id def-456"
	s := New(service.New(erroringBackend{err: errors.New(internalDetail)}, cost.NewGuard(0, &bytes.Buffer{})))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_memory",
		Arguments: map[string]any{"query": "x"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatal("search_memory did not report a tool error for a failing backend")
	}
	if containsText(result, "def-456") || containsText(result, "ThrottlingException") {
		t.Fatalf("backend error detail leaked to the caller: %+v", result.Content)
	}
	if !containsText(result, "backend unavailable") {
		t.Errorf("search_memory error does not mention backend unavailable: %+v", result.Content)
	}
}

// TestBudgetExceededMessageDoesNotLeakDetail extends
// TestBudgetExceededRejectsToolCalls: the rejection must name the
// exceeded budget generically, without disclosing the exact cap or
// cumulative call count to an unauthenticated caller.
func TestBudgetExceededMessageDoesNotLeakDetail(t *testing.T) {
	budget := cost.NewGuard(cost.EstimatedCostPerCallGBP, &bytes.Buffer{})
	ts := newTestServer(t, budget)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": "first call"},
	}); err != nil {
		t.Fatalf("CallTool (first): %v", err)
	}

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
	if containsText(second, "gbp") || containsText(second, "estimated") || containsText(second, "cap") {
		t.Errorf("budget rejection leaks cap/estimate detail: %+v", second.Content)
	}
}

// TestSaveMemoryRejectsOversizedContent pins save_memory's content length
// ceiling: a caller-caused validation error, so it is fine to be specific
// (unlike a backend error).
func TestSaveMemoryRejectsOversizedContent(t *testing.T) {
	ts := newTestServer(t, nil)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	oversized := strings.Repeat("a", service.MaxContentBytes+1)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": oversized},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatal("save_memory accepted content over the byte ceiling")
	}
}

// TestSearchMemoryClampsLimitToMax pins the fix for a caller-supplied
// limit large enough to overflow an int32 conversion (e.g. AgentCore's
// TopK/MaxResults) if forwarded unclamped.
func TestSearchMemoryClampsLimitToMax(t *testing.T) {
	rec := &recordingBackend{}
	s := New(service.New(rec, cost.NewGuard(0, &bytes.Buffer{})))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_memory",
		Arguments: map[string]any{"query": "x", "limit": 2147483648},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("search_memory rejected an oversized limit instead of clamping it: %+v", result.Content)
	}
	if got := rec.lastLimit(); got != service.MaxSearchLimit {
		t.Errorf("Limit forwarded to the backend = %d, want the %d ceiling", got, service.MaxSearchLimit)
	}
}

// TestNewWithNilBudgetDoesNotPanic pins New's documented nil-budget
// default: a future caller passing nil must get an uncapped guard, not a
// panic on the first save_memory/search_memory call.
func TestNewWithNilBudgetDoesNotPanic(t *testing.T) {
	s := New(service.New(backend.NewMemoryBackend(), nil))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "save_memory",
		Arguments: map[string]any{"content": "x"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("save_memory with a nil budget was rejected: %+v", result.Content)
	}
}

// TestCrossOriginRequestsAreRejected pins the CORS/DNS-rebinding
// protection: a request whose Origin doesn't match Host, sent with a
// browser's Sec-Fetch-Site header, must be rejected outright. Stirrup's
// own client (TestStirrupCompatibleRawJSONRPC) sends neither header and
// is unaffected.
func TestCrossOriginRequestsAreRejected(t *testing.T) {
	ts := newTestServer(t, nil)

	req, err := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin request got HTTP %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestStirrupCompatibleRawJSONRPCMalformedInputs extends the raw JSON-RPC
// harness in TestStirrupCompatibleRawJSONRPC with inputs a real client
// should never send but a hostile or buggy one might: the server must
// respond (not hang or crash) and remain usable afterward.
func TestStirrupCompatibleRawJSONRPCMalformedInputs(t *testing.T) {
	ts := newTestServer(t, nil)

	post := func(t *testing.T, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}

	t.Run("invalid JSON body", func(t *testing.T) {
		resp := post(t, `{not valid json`)
		defer resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("HTTP %d, want a 4xx client error for an unparseable body", resp.StatusCode)
		}
	})

	t.Run("missing jsonrpc version", func(t *testing.T) {
		resp := post(t, `{"id":1,"method":"tools/list"}`)
		defer resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("HTTP %d, want a 4xx client error for a missing jsonrpc version", resp.StatusCode)
		}
	})

	t.Run("wrong jsonrpc version", func(t *testing.T) {
		resp := post(t, `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`)
		defer resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("HTTP %d, want a 4xx client error for the wrong jsonrpc version", resp.StatusCode)
		}
	})

	t.Run("missing method", func(t *testing.T) {
		resp := post(t, `{"jsonrpc":"2.0","id":1}`)
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Errorf("HTTP %d, want no server error for a request with no method", resp.StatusCode)
		}
	})

	t.Run("tools/call names an unknown tool", func(t *testing.T) {
		resp := post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d, want 200 with a JSON-RPC error body", resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if _, ok := out["error"]; !ok {
			t.Fatalf("response has no JSON-RPC error for an unknown tool: %+v", out)
		}
	})

	// The server must still be responsive after all of the above.
	resp := post(t, `{"jsonrpc":"2.0","id":99,"method":"tools/list"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server unresponsive after malformed input: HTTP %d", resp.StatusCode)
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
