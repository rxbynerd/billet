// Package mcpserver exposes Billet's tool surface as an MCP server over
// Streamable HTTP: the save_memory and search_memory tools Stirrup's
// harness calls directly (docs/DECISIONS.md, "two transports"). The
// tool semantics live in internal/service; this package owns only the
// MCP protocol adaptation.
package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rxbynerd/billet/internal/service"
)

const (
	implementationName = "billet"

	// maxRequestBodyBytes bounds the entire MCP request body the
	// Streamable HTTP handler will read, set explicitly rather than
	// relying on the SDK's current default. It has headroom over
	// service.MaxContentBytes for the JSON-RPC/tool-call envelope and
	// escaping overhead around a single save_memory call.
	maxRequestBodyBytes = 1 << 20 // 1 MiB
)

// Version is the Billet build version reported in the MCP Implementation
// handshake. Overridable at link time (-ldflags -X); "dev" otherwise.
var Version = "dev"

// Server adapts a service.Service to the MCP save_memory/search_memory
// tool surface. Validation, budget gating, and the generic error policy
// all live in the service, shared with the RPC transport.
type Server struct {
	svc *service.Service
}

// New returns a Server delegating to svc.
func New(svc *service.Service) *Server {
	return &Server{svc: svc}
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
	res, err := s.svc.SaveMemory(ctx, in.Content, in.Kind)
	if err != nil {
		return nil, saveMemoryOutput{}, err
	}
	return nil, saveMemoryOutput{MemoryID: res.MemoryID, Accepted: res.Accepted}, nil
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
	records, err := s.svc.SearchMemory(ctx, in.Query, in.Limit)
	if err != nil {
		return nil, searchMemoryOutput{}, err
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
