// Package mcpserver exposes a Backend as an MCP server over Streamable
// HTTP: the save_memory and search_memory tools Stirrup's harness calls
// (docs/DECISIONS.md, "transport: MCP server, not connect-go/gRPC").
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/cost"
)

const (
	implementationName = "billet"

	// defaultSearchLimit matches Bedrock's numberOfResults convention
	// (PROPOSAL.md) and the documented search_memory default.
	defaultSearchLimit = 5
	// maxSearchLimit bounds search_memory's caller-supplied limit. Without
	// a ceiling, a value near or above math.MaxInt32 would silently wrap
	// to a negative TopK/MaxResults once a backend converts it to int32.
	maxSearchLimit = 100

	// maxContentBytes bounds save_memory's content field. This is a
	// caller-facing limit (returned as a normal validation error), not an
	// attempt to size the backend's actual storage limits.
	maxContentBytes = 256 << 10 // 256 KiB

	// maxRequestBodyBytes bounds the entire MCP request body the
	// Streamable HTTP handler will read, set explicitly rather than
	// relying on the SDK's current default. It has headroom over
	// maxContentBytes for the JSON-RPC/tool-call envelope and escaping
	// overhead around a single save_memory call.
	maxRequestBodyBytes = 1 << 20 // 1 MiB
)

// errBudgetExceeded, errSaveBackendUnavailable, and
// errSearchBackendUnavailable are the only detail an MCP caller ever sees
// for a budget rejection or a backend failure. The MCP endpoint is
// unauthenticated by default (docs/security.md), so the real detail
// (the exact cap and call count; AWS account IDs, ARNs, and request IDs
// on a backend error) is logged server-side via log/slog instead of
// returned to the caller.
var (
	errBudgetExceeded           = errors.New("budget exceeded")
	errSaveBackendUnavailable   = errors.New("save_memory: backend unavailable")
	errSearchBackendUnavailable = errors.New("search_memory: backend unavailable")
)

// Version is the Billet build version reported in the MCP Implementation
// handshake. Overridable at link time (-ldflags -X); "dev" otherwise.
var Version = "dev"

// Server adapts a Backend to the MCP save_memory/search_memory tool
// surface, gated by a cost.Guard. Namespace and session identity are not
// part of Server: they are already baked into the Backend it was built
// with (internal/backend's construction-time binding).
type Server struct {
	backend backend.Backend
	budget  *cost.Guard
}

// New returns a Server delegating to b and gated by budget. A nil budget
// defaults to an uncapped guard (equivalent to cost.NewGuard(0, nil))
// rather than panicking on first use.
func New(b backend.Backend, budget *cost.Guard) *Server {
	if budget == nil {
		budget = cost.NewGuard(0, nil)
	}
	return &Server{backend: b, budget: budget}
}

// Handler returns the http.Handler serving Billet's MCP endpoint: the
// Streamable HTTP transport, stateless and JSON-only.
//
// Stirrup's actual MCP client (harness/internal/mcp/client.go) sends no
// Accept header and never issues an "initialize" call before tools/list —
// it is a minimal, non-fully-compliant Streamable HTTP client, not the
// reference client the SDK expects. Stateless mode tolerates the missing
// initialize handshake (it synthesizes default session state per
// request); JSONResponse avoids SSE framing the client cannot parse (it
// does a raw json.Unmarshal of the response body); and
// acceptAnyMediaType supplies the Accept header the client omits, which
// the SDK otherwise requires and rejects with 400 when absent. All three
// were verified empirically against github.com/modelcontextprotocol/go-sdk
// v1.7.0 (docs/DECISIONS.md).
func (s *Server) Handler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: implementationName, Version: Version}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "save_memory",
		Description: "Persist a memory (a raw turn or an explicit fact) to Billet's configured backend.",
	}, s.saveMemory)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_memory",
		Description: "Search previously saved memories and return the best-matching records.",
	}, s.searchMemory)

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:           true,
			JSONResponse:        true,
			MaxRequestBodyBytes: maxRequestBodyBytes,
		},
	)

	// CORS/DNS-rebinding protection: rejects a cross-origin browser
	// request before it reaches the handler at all. The SDK's own
	// StreamableHTTPOptions.CrossOriginProtection field is deprecated in
	// favour of this external wrapping.
	protected := http.NewCrossOriginProtection().Handler(handler)

	return acceptAnyMediaType(protected)
}

// acceptAnyMediaType supplies a compliant Accept header when the request
// has none, so a client that omits it (Stirrup's) is not rejected by the
// Streamable HTTP transport's Accept negotiation.
func acceptAnyMediaType(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "" {
			r.Header.Set("Accept", "application/json, text/event-stream")
		}
		h.ServeHTTP(w, r)
	})
}

type saveMemoryInput struct {
	Content string `json:"content" jsonschema:"the raw turn content or an explicit fact to remember; max 256 KiB"`
	Kind    string `json:"kind,omitempty" jsonschema:"optional hint: 'event' or 'fact' (defaults to 'event')"`
}

type saveMemoryOutput struct {
	MemoryID string `json:"memory_id"`
	Accepted bool   `json:"accepted"`
}

func (s *Server) saveMemory(ctx context.Context, _ *mcp.CallToolRequest, in saveMemoryInput) (*mcp.CallToolResult, saveMemoryOutput, error) {
	if err := s.budget.Allow(); err != nil {
		slog.Warn("save_memory rejected by budget guard", "error", err)
		return nil, saveMemoryOutput{}, errBudgetExceeded
	}

	if strings.TrimSpace(in.Content) == "" {
		return nil, saveMemoryOutput{}, errors.New("save_memory: content must not be empty")
	}
	if len(in.Content) > maxContentBytes {
		return nil, saveMemoryOutput{}, fmt.Errorf("save_memory: content exceeds the %d byte limit", maxContentBytes)
	}

	kind := backend.KindEvent
	switch in.Kind {
	case "":
	case string(backend.KindEvent):
		kind = backend.KindEvent
	case string(backend.KindFact):
		kind = backend.KindFact
	default:
		return nil, saveMemoryOutput{}, fmt.Errorf("save_memory: kind %q is not \"event\" or \"fact\"", in.Kind)
	}

	id, err := s.backend.Save(ctx, backend.SaveRequest{Content: in.Content, Kind: kind})
	if err != nil {
		slog.Error("save_memory: backend Save failed", "error", err)
		return nil, saveMemoryOutput{}, errSaveBackendUnavailable
	}
	return nil, saveMemoryOutput{MemoryID: id, Accepted: true}, nil
}

type searchMemoryInput struct {
	Query string `json:"query" jsonschema:"the search query"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of results (default 5, capped at 100)"`
}

type memoryRecord struct {
	MemoryID  string  `json:"memory_id"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

type searchMemoryOutput struct {
	Records []memoryRecord `json:"records"`
}

func (s *Server) searchMemory(ctx context.Context, _ *mcp.CallToolRequest, in searchMemoryInput) (*mcp.CallToolResult, searchMemoryOutput, error) {
	if err := s.budget.Allow(); err != nil {
		slog.Warn("search_memory rejected by budget guard", "error", err)
		return nil, searchMemoryOutput{}, errBudgetExceeded
	}

	if strings.TrimSpace(in.Query) == "" {
		return nil, searchMemoryOutput{}, errors.New("search_memory: query must not be empty")
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultSearchLimit
	case limit > maxSearchLimit:
		limit = maxSearchLimit
	}

	records, err := s.backend.Search(ctx, backend.SearchRequest{Query: in.Query, Limit: limit})
	if err != nil {
		slog.Error("search_memory: backend Search failed", "error", err)
		return nil, searchMemoryOutput{}, errSearchBackendUnavailable
	}

	out := searchMemoryOutput{Records: make([]memoryRecord, len(records))}
	for i, r := range records {
		out.Records[i] = memoryRecord{
			MemoryID:  r.MemoryID,
			Content:   r.Content,
			Score:     r.Score,
			CreatedAt: r.CreatedAt,
		}
	}
	return nil, out, nil
}
