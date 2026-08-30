package backend

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// fakeAgentCoreAPI is a scriptable agentCoreAPI double: no HTTP, no
// SigV4 signing, no real AWS endpoint. It captures the exact request
// shape Billet sends so tests assert against it directly.
type fakeAgentCoreAPI struct {
	createEventInput *bedrockagentcore.CreateEventInput
	createEventOut   *bedrockagentcore.CreateEventOutput
	createEventErr   error

	retrieveInput *bedrockagentcore.RetrieveMemoryRecordsInput
	retrieveOut   *bedrockagentcore.RetrieveMemoryRecordsOutput
	retrieveErr   error
}

func (f *fakeAgentCoreAPI) CreateEvent(_ context.Context, params *bedrockagentcore.CreateEventInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CreateEventOutput, error) {
	f.createEventInput = params
	if f.createEventErr != nil {
		return nil, f.createEventErr
	}
	return f.createEventOut, nil
}

func (f *fakeAgentCoreAPI) RetrieveMemoryRecords(_ context.Context, params *bedrockagentcore.RetrieveMemoryRecordsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.RetrieveMemoryRecordsOutput, error) {
	f.retrieveInput = params
	if f.retrieveErr != nil {
		return nil, f.retrieveErr
	}
	return f.retrieveOut, nil
}

func newTestAgentCoreMemory(api agentCoreAPI) *AgentCoreMemory {
	return &AgentCoreMemory{
		api:       api,
		memoryID:  "mem-resource-123",
		namespace: "test-namespace",
		sessionID: "test-session",
	}
}

func TestAgentCoreMemorySaveShapesCreateEvent(t *testing.T) {
	fake := &fakeAgentCoreAPI{
		createEventOut: &bedrockagentcore.CreateEventOutput{
			Event: &types.Event{EventId: aws.String("evt-1")},
		},
	}
	a := newTestAgentCoreMemory(fake)

	id, err := a.Save(context.Background(), SaveRequest{Content: "the horse likes carrots", Kind: KindFact})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if id != "evt-1" {
		t.Errorf("Save id = %q, want evt-1", id)
	}

	req := fake.createEventInput
	if req == nil {
		t.Fatal("CreateEvent was not called")
	}
	if aws.ToString(req.MemoryId) != "mem-resource-123" {
		t.Errorf("MemoryId = %q, want mem-resource-123", aws.ToString(req.MemoryId))
	}
	if aws.ToString(req.ActorId) != "test-namespace" {
		t.Errorf("ActorId = %q, want the bound namespace", aws.ToString(req.ActorId))
	}
	if aws.ToString(req.SessionId) != "test-session" {
		t.Errorf("SessionId = %q, want the bound session id", aws.ToString(req.SessionId))
	}
	if req.EventTimestamp == nil || req.EventTimestamp.After(time.Now().UTC()) {
		t.Errorf("EventTimestamp = %v, want a recent timestamp", req.EventTimestamp)
	}
	if len(req.Payload) != 1 {
		t.Fatalf("Payload has %d entries, want 1", len(req.Payload))
	}
	jsonPayload, ok := req.Payload[0].(*types.PayloadTypeMemberJson)
	if !ok {
		t.Fatalf("Payload[0] = %T, want *types.PayloadTypeMemberJson", req.Payload[0])
	}
	decoded := decodePayloadDocument(t, jsonPayload)
	if decoded["content"] != "the horse likes carrots" || decoded["kind"] != "fact" {
		t.Errorf("decoded payload = %+v, want content/kind from the request", decoded)
	}
}

func TestAgentCoreMemorySaveDefaultsKindToEvent(t *testing.T) {
	fake := &fakeAgentCoreAPI{
		createEventOut: &bedrockagentcore.CreateEventOutput{
			Event: &types.Event{EventId: aws.String("evt-2")},
		},
	}
	a := newTestAgentCoreMemory(fake)

	if _, err := a.Save(context.Background(), SaveRequest{Content: "no kind given"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	jsonPayload := fake.createEventInput.Payload[0].(*types.PayloadTypeMemberJson)
	decoded := decodePayloadDocument(t, jsonPayload)
	if decoded["kind"] != string(KindEvent) {
		t.Errorf("kind = %q, want default %q", decoded["kind"], KindEvent)
	}
}

// decodePayloadDocument renders the document.Interface content of a JSON
// event payload to the JSON bytes AgentCore would actually receive, via
// MarshalSmithyDocument (the request-side path; UnmarshalSmithyDocument
// is for response documents and is not exercised on a value built with
// NewLazyDocument).
func decodePayloadDocument(t *testing.T, p *types.PayloadTypeMemberJson) map[string]interface{} {
	t.Helper()
	marshaler, ok := p.Value.Content.(interface {
		MarshalSmithyDocument() ([]byte, error)
	})
	if !ok {
		t.Fatalf("payload content %T does not implement MarshalSmithyDocument", p.Value.Content)
	}
	raw, err := marshaler.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("MarshalSmithyDocument: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal payload JSON %q: %v", raw, err)
	}
	return decoded
}

func TestAgentCoreMemorySavePropagatesError(t *testing.T) {
	fake := &fakeAgentCoreAPI{createEventErr: errors.New("boom")}
	a := newTestAgentCoreMemory(fake)

	if _, err := a.Save(context.Background(), SaveRequest{Content: "x"}); err == nil {
		t.Fatal("Save succeeded despite a CreateEvent error")
	}
}

func TestAgentCoreMemorySaveRejectsMissingEventID(t *testing.T) {
	fake := &fakeAgentCoreAPI{createEventOut: &bedrockagentcore.CreateEventOutput{Event: &types.Event{}}}
	a := newTestAgentCoreMemory(fake)

	if _, err := a.Save(context.Background(), SaveRequest{Content: "x"}); err == nil {
		t.Fatal("Save succeeded despite a missing EventId in the response")
	}
}

func TestAgentCoreMemorySearchShapesRetrieveMemoryRecords(t *testing.T) {
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := &fakeAgentCoreAPI{
		retrieveOut: &bedrockagentcore.RetrieveMemoryRecordsOutput{
			MemoryRecordSummaries: []types.MemoryRecordSummary{
				{
					MemoryRecordId: aws.String("rec-1"),
					Content:        &types.MemoryContentMemberText{Value: "the paddock gate was left open"},
					Score:          aws.Float64(0.87),
					CreatedAt:      aws.Time(createdAt),
				},
			},
		},
	}
	a := newTestAgentCoreMemory(fake)

	records, err := a.Search(context.Background(), SearchRequest{Query: "paddock gate", Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	req := fake.retrieveInput
	if req == nil {
		t.Fatal("RetrieveMemoryRecords was not called")
	}
	if aws.ToString(req.MemoryId) != "mem-resource-123" {
		t.Errorf("MemoryId = %q", aws.ToString(req.MemoryId))
	}
	if aws.ToString(req.Namespace) != "test-namespace" {
		t.Errorf("Namespace = %q, want the bound namespace", aws.ToString(req.Namespace))
	}
	if aws.ToInt32(req.MaxResults) != 3 {
		t.Errorf("MaxResults = %d, want 3", aws.ToInt32(req.MaxResults))
	}
	if req.SearchCriteria == nil || aws.ToString(req.SearchCriteria.SearchQuery) != "paddock gate" {
		t.Errorf("SearchCriteria = %+v, want SearchQuery %q", req.SearchCriteria, "paddock gate")
	}
	if req.SearchCriteria == nil || aws.ToInt32(req.SearchCriteria.TopK) != 3 {
		t.Errorf("SearchCriteria.TopK = %v, want 3", req.SearchCriteria)
	}

	if len(records) != 1 {
		t.Fatalf("Search returned %d records, want 1", len(records))
	}
	got := records[0]
	if got.MemoryID != "rec-1" {
		t.Errorf("MemoryID = %q, want rec-1", got.MemoryID)
	}
	if got.Content != "the paddock gate was left open" {
		t.Errorf("Content = %q", got.Content)
	}
	if got.Score != 0.87 {
		t.Errorf("Score = %v, want 0.87", got.Score)
	}
	if got.CreatedAt != createdAt.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", got.CreatedAt, createdAt.Format(time.RFC3339))
	}
}

func TestAgentCoreMemorySearchDefaultLimit(t *testing.T) {
	fake := &fakeAgentCoreAPI{retrieveOut: &bedrockagentcore.RetrieveMemoryRecordsOutput{}}
	a := newTestAgentCoreMemory(fake)

	if _, err := a.Search(context.Background(), SearchRequest{Query: "x"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if aws.ToInt32(fake.retrieveInput.MaxResults) != 5 {
		t.Errorf("MaxResults = %d, want default 5", aws.ToInt32(fake.retrieveInput.MaxResults))
	}
}

func TestAgentCoreMemorySearchEmptyResult(t *testing.T) {
	fake := &fakeAgentCoreAPI{retrieveOut: &bedrockagentcore.RetrieveMemoryRecordsOutput{}}
	a := newTestAgentCoreMemory(fake)

	records, err := a.Search(context.Background(), SearchRequest{Query: "nothing stored"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("Search returned %d records, want 0", len(records))
	}
}

func TestAgentCoreMemorySearchPropagatesError(t *testing.T) {
	fake := &fakeAgentCoreAPI{retrieveErr: errors.New("boom")}
	a := newTestAgentCoreMemory(fake)

	if _, err := a.Search(context.Background(), SearchRequest{Query: "x"}); err == nil {
		t.Fatal("Search succeeded despite a RetrieveMemoryRecords error")
	}
}

func TestNewAgentCoreMemoryRequiresFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  AgentCoreMemoryConfig
	}{
		{"missing region", AgentCoreMemoryConfig{MemoryID: "m", Namespace: "n"}},
		{"missing memoryId", AgentCoreMemoryConfig{Region: "eu-west-2", Namespace: "n"}},
		{"missing namespace", AgentCoreMemoryConfig{Region: "eu-west-2", MemoryID: "m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewAgentCoreMemory(context.Background(), tt.cfg, nil); err == nil {
				t.Fatal("NewAgentCoreMemory accepted an incomplete config")
			}
		})
	}
}

func TestNewAgentCoreMemoryFailsClosedOnUnresolvableCredentials(t *testing.T) {
	cfg := AgentCoreMemoryConfig{
		Region:         "eu-west-2",
		MemoryID:       "m",
		Namespace:      "n",
		CredentialsRef: "secret://BILLET_TEST_UNSET_PROFILE_VAR",
	}
	if _, err := NewAgentCoreMemory(context.Background(), cfg, testResolver{}); err == nil {
		t.Fatal("NewAgentCoreMemory succeeded despite an unresolvable credentialsRef")
	}
}

// testResolver resolves nothing; every reference fails, exercising the
// fail-closed construction path without needing internal/secret as a
// dependency of internal/backend's tests.
type testResolver struct{}

func (testResolver) Resolve(context.Context, string) (string, error) {
	return "", errors.New("test resolver: not found")
}
