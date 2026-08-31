package rpcserver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	billetv1 "github.com/rxbynerd/billet/gen/billet/v1"
	"github.com/rxbynerd/billet/gen/billet/v1/billetv1connect"
	"github.com/rxbynerd/billet/internal/backend"
	"github.com/rxbynerd/billet/internal/cost"
	"github.com/rxbynerd/billet/internal/service"
)

type erroringBackend struct {
	err error
}

func (e erroringBackend) Save(context.Context, backend.SaveRequest) (string, error) {
	return "", e.err
}

func (e erroringBackend) Search(context.Context, backend.SearchRequest) ([]backend.Record, error) {
	return nil, e.err
}

type recordingBackend struct {
	mu       sync.Mutex
	lastSave backend.SaveRequest
}

func (r *recordingBackend) Save(_ context.Context, req backend.SaveRequest) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSave = req
	return "rec-id", nil
}

func (r *recordingBackend) Search(context.Context, backend.SearchRequest) ([]backend.Record, error) {
	return nil, nil
}

func (r *recordingBackend) lastKind() backend.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSave.Kind
}

func newTestServer(t *testing.T, b backend.Backend, budget *cost.Guard) *httptest.Server {
	t.Helper()
	if b == nil {
		b = backend.NewMemoryBackend()
	}
	if budget == nil {
		budget = cost.NewGuard(0, &bytes.Buffer{})
	}
	s := New(service.New(b, budget))
	ts := httptest.NewUnstartedServer(s.Handler())
	// Mirror `billet serve`'s RPC listener protocol set, so the gRPC
	// tests exercise the same cleartext-HTTP/2 path a real deployment
	// serves.
	ts.Config.Protocols = Protocols()
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// h2cClient returns an HTTP client speaking HTTP/2 over cleartext, the
// transport a real gRPC client needs against Billet's cleartext RPC
// listener.
func h2cClient() *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{Protocols: p},
	}
}

func saveThenSearch(t *testing.T, client billetv1connect.MemoryServiceClient) {
	t.Helper()
	ctx := context.Background()

	saved, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{
		Content: "the paddock gate was left open",
		Kind:    billetv1.Kind_KIND_FACT,
	}))
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if saved.Msg.GetMemoryId() == "" || !saved.Msg.GetAccepted() {
		t.Fatalf("SaveMemory response = %+v, want a memory_id and accepted=true", saved.Msg)
	}

	searched, err := client.SearchMemory(ctx, connect.NewRequest(&billetv1.SearchMemoryRequest{
		Query: "paddock gate",
	}))
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	records := searched.Msg.GetRecords()
	if len(records) != 1 || records[0].GetMemoryId() != saved.Msg.GetMemoryId() {
		t.Fatalf("SearchMemory response = %+v, want the saved record first", records)
	}
}

// TestSaveThenSearchConnectProtocol drives the server end to end over
// the Connect protocol on HTTP/1.1, the path a connect-go control-plane
// client takes by default.
func TestSaveThenSearchConnectProtocol(t *testing.T) {
	ts := newTestServer(t, nil, nil)
	saveThenSearch(t, billetv1connect.NewMemoryServiceClient(ts.Client(), ts.URL))
}

// TestSaveThenSearchGRPCOverH2C drives the server end to end over the
// gRPC protocol on cleartext HTTP/2, proving the h2c wrapping works for
// a real gRPC caller without TLS in front of Billet.
func TestSaveThenSearchGRPCOverH2C(t *testing.T) {
	ts := newTestServer(t, nil, nil)
	saveThenSearch(t, billetv1connect.NewMemoryServiceClient(h2cClient(), ts.URL, connect.WithGRPC()))
}

// TestKindEnumMapsToBackendKind protects the enum-to-kind mapping,
// including the UNSPECIFIED-defaults-to-event rule.
func TestKindEnumMapsToBackendKind(t *testing.T) {
	rec := &recordingBackend{}
	ts := newTestServer(t, rec, nil)
	client := billetv1connect.NewMemoryServiceClient(ts.Client(), ts.URL)
	ctx := context.Background()

	if _, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{
		Content: "x", Kind: billetv1.Kind_KIND_FACT,
	})); err != nil {
		t.Fatalf("SaveMemory(KIND_FACT): %v", err)
	}
	if got := rec.lastKind(); got != backend.KindFact {
		t.Errorf("KIND_FACT forwarded as %q, want %q", got, backend.KindFact)
	}

	if _, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{
		Content: "x",
	})); err != nil {
		t.Fatalf("SaveMemory(unspecified kind): %v", err)
	}
	if got := rec.lastKind(); got != backend.KindEvent {
		t.Errorf("KIND_UNSPECIFIED forwarded as %q, want %q", got, backend.KindEvent)
	}
}

// TestValidationErrorsMapToInvalidArgument protects the status-code
// mapping for caller-caused errors on the RPC transport.
func TestValidationErrorsMapToInvalidArgument(t *testing.T) {
	ts := newTestServer(t, nil, nil)
	client := billetv1connect.NewMemoryServiceClient(ts.Client(), ts.URL)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"empty content", func() error {
			_, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{Content: "  "}))
			return err
		}},
		{"unknown kind enum value", func() error {
			_, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{Content: "x", Kind: billetv1.Kind(99)}))
			return err
		}},
		{"oversized content", func() error {
			_, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{Content: strings.Repeat("a", service.MaxContentBytes+1)}))
			return err
		}},
		{"empty query", func() error {
			_, err := client.SearchMemory(ctx, connect.NewRequest(&billetv1.SearchMemoryRequest{Query: " "}))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("code = %v (err %v), want invalid_argument", connect.CodeOf(err), err)
			}
		})
	}
}

// TestBudgetExceededMapsToResourceExhausted protects the budget
// rejection path: resource_exhausted, with no cap or call-count detail.
func TestBudgetExceededMapsToResourceExhausted(t *testing.T) {
	budget := cost.NewGuard(cost.EstimatedCostPerCallGBP, &bytes.Buffer{})
	ts := newTestServer(t, nil, budget)
	client := billetv1connect.NewMemoryServiceClient(ts.Client(), ts.URL)
	ctx := context.Background()

	if _, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{Content: "first"})); err != nil {
		t.Fatalf("first call: %v", err)
	}

	_, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{Content: "second"}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("code = %v (err %v), want resource_exhausted", connect.CodeOf(err), err)
	}
	msg := strings.ToLower(err.Error())
	for _, leak := range []string{"gbp", "estimated", "cap"} {
		if strings.Contains(msg, leak) {
			t.Errorf("budget rejection leaks %q: %v", leak, err)
		}
	}
}

// TestBackendErrorMapsToUnavailableGeneric protects the error policy on
// the RPC transport: a failing backend surfaces unavailable with the
// generic message, never the underlying detail.
func TestBackendErrorMapsToUnavailableGeneric(t *testing.T) {
	const internalDetail = "AccessDeniedException: arn:aws:iam::123456789012:role/billet is not authorized (RequestId: abc-123)"
	ts := newTestServer(t, erroringBackend{err: errors.New(internalDetail)}, nil)
	client := billetv1connect.NewMemoryServiceClient(ts.Client(), ts.URL)
	ctx := context.Background()

	_, err := client.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{Content: "x"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v (err %v), want unavailable", connect.CodeOf(err), err)
	}
	if strings.Contains(err.Error(), "123456789012") || strings.Contains(err.Error(), "arn:aws") {
		t.Fatalf("backend error detail leaked to the caller: %v", err)
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("error does not carry the generic message: %v", err)
	}
}

// TestCrossOriginRequestsAreRejected pins the CORS/DNS-rebinding
// protection on the RPC endpoint, matching the MCP endpoint's.
func TestCrossOriginRequestsAreRejected(t *testing.T) {
	ts := newTestServer(t, nil, nil)

	req, err := http.NewRequest(http.MethodPost, ts.URL+billetv1connect.MemoryServiceSaveMemoryProcedure, bytes.NewReader([]byte(`{}`)))
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
