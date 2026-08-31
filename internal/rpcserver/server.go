// Package rpcserver exposes Billet's tool surface as a Connect RPC
// service (billet.v1.MemoryService) speaking the Connect, gRPC, and
// gRPC-Web protocols, for control-plane-proxied deployments where the
// agent environment has no direct network access to Billet
// (docs/DECISIONS.md, "two transports"). The tool semantics live in
// internal/service; this package owns only the RPC protocol adaptation.
package rpcserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"

	billetv1 "github.com/rxbynerd/billet/gen/billet/v1"
	"github.com/rxbynerd/billet/gen/billet/v1/billetv1connect"
	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/service"
)

// maxMessageBytes bounds a single RPC message, mirroring the MCP
// transport's request-body ceiling: headroom over service.MaxContentBytes
// for the envelope around one save_memory call.
const maxMessageBytes = 1 << 20 // 1 MiB

// Server adapts a service.Service to billet.v1.MemoryService.
// Validation, budget gating, and the generic error policy all live in
// the service, shared with the MCP transport.
type Server struct {
	svc *service.Service
}

// New returns a Server delegating to svc.
func New(svc *service.Service) *Server {
	return &Server{svc: svc}
}

// Handler returns the http.Handler serving Billet's RPC endpoint.
// Connect and gRPC-Web clients work over HTTP/1.1; gRPC clients require
// HTTP/2, so the http.Server hosting this handler must enable
// unencrypted HTTP/2 (http.Protocols.SetUnencryptedHTTP2) when there is
// no TLS termination in front of Billet — Protocols() supplies the
// right value.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	path, handler := billetv1connect.NewMemoryServiceHandler(s,
		connect.WithReadMaxBytes(maxMessageBytes),
	)
	mux.Handle(path, handler)

	// CORS/DNS-rebinding protection, matching the MCP endpoint: a
	// cross-origin browser request is rejected before it reaches the
	// handler. Server-side RPC clients send neither Origin nor
	// Sec-Fetch-Site and are unaffected.
	return http.NewCrossOriginProtection().Handler(mux)
}

// Protocols returns the protocol set an http.Server hosting Handler
// should accept: HTTP/1.1 for Connect and gRPC-Web, plus unencrypted
// HTTP/2 so cleartext gRPC clients can connect.
func Protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// SaveMemory implements billetv1connect.MemoryServiceHandler.
func (s *Server) SaveMemory(ctx context.Context, req *connect.Request[billetv1.SaveMemoryRequest]) (*connect.Response[billetv1.SaveMemoryResponse], error) {
	kind, err := kindString(req.Msg.GetKind())
	if err != nil {
		return nil, err
	}

	res, err := s.svc.SaveMemory(ctx, req.Msg.GetContent(), kind)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&billetv1.SaveMemoryResponse{
		MemoryId: res.MemoryID,
		Accepted: res.Accepted,
	}), nil
}

// SearchMemory implements billetv1connect.MemoryServiceHandler.
func (s *Server) SearchMemory(ctx context.Context, req *connect.Request[billetv1.SearchMemoryRequest]) (*connect.Response[billetv1.SearchMemoryResponse], error) {
	records, err := s.svc.SearchMemory(ctx, req.Msg.GetQuery(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, rpcError(err)
	}

	out := &billetv1.SearchMemoryResponse{
		Records: make([]*billetv1.MemoryRecord, len(records)),
	}
	for i, r := range records {
		out.Records[i] = &billetv1.MemoryRecord{
			MemoryId:  r.MemoryID,
			Content:   r.Content,
			Score:     r.Score,
			CreatedAt: r.CreatedAt,
		}
	}
	return connect.NewResponse(out), nil
}

// kindString maps the proto Kind enum onto the service's wire-level kind
// hint. An enum value outside the schema is a caller error.
func kindString(k billetv1.Kind) (string, error) {
	switch k {
	case billetv1.Kind_KIND_UNSPECIFIED:
		return "", nil
	case billetv1.Kind_KIND_EVENT:
		return string(backend.KindEvent), nil
	case billetv1.Kind_KIND_FACT:
		return string(backend.KindFact), nil
	default:
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("save_memory: kind %d is not a known Kind value", k))
	}
}

// rpcError maps a service error onto a Connect status code. The service
// already enforces the caller-facing error policy (generic messages for
// budget and backend failures, specific messages for caller-caused
// validation errors), so the message is passed through as-is.
func rpcError(err error) error {
	var invalid *service.InvalidInputError
	switch {
	case errors.As(err, &invalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, service.ErrBudgetExceeded):
		return connect.NewError(connect.CodeResourceExhausted, err)
	default:
		return connect.NewError(connect.CodeUnavailable, err)
	}
}
