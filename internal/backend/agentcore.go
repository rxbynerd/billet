package backend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"

	"github.com/rxbynerd/billet/internal/secret"
)

// agentCoreAPI is the subset of *bedrockagentcore.Client Billet calls,
// declared locally so tests substitute a mock directly instead of
// reimplementing SigV4-signed HTTP or a fake AWS endpoint
// (docs/DECISIONS.md).
type agentCoreAPI interface {
	CreateEvent(ctx context.Context, params *bedrockagentcore.CreateEventInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CreateEventOutput, error)
	RetrieveMemoryRecords(ctx context.Context, params *bedrockagentcore.RetrieveMemoryRecordsInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.RetrieveMemoryRecordsOutput, error)
}

// AgentCoreMemoryConfig configures the AgentCoreMemory backend.
type AgentCoreMemoryConfig struct {
	// Region is the AWS region hosting the AgentCore Memory resource.
	Region string
	// MemoryID identifies the AgentCore Memory resource.
	MemoryID string
	// Namespace is Billet's namespace, bound to AgentCore's actorId on
	// every CreateEvent and used as the RetrieveMemoryRecords namespace
	// filter (docs/DECISIONS.md; see TODO.md on the latter's namespace
	// template assumption).
	Namespace string
	// SessionID scopes AgentCore's short-term/event memory. Empty
	// generates a fresh one for this backend's lifetime — the v1
	// simplification described in docs/DECISIONS.md ("no session_id
	// parameter on the tools").
	SessionID string
	// CredentialsRef is a secret:// reference resolving to the AWS
	// shared-config profile name to use. Empty uses the SDK's ordinary
	// default credential chain.
	CredentialsRef string
}

// AgentCoreMemory is the v1 cloud Backend: AWS Bedrock AgentCore Memory.
// save_memory maps to CreateEvent (long-term extraction then runs
// asynchronously server-side); search_memory maps to
// RetrieveMemoryRecords (semantic search over already-extracted
// records). It is best-effort in this codebase — there is no live AWS
// account to test against (TODO.md) — so construction fails closed on
// any credential or config error rather than falling back silently.
type AgentCoreMemory struct {
	api       agentCoreAPI
	memoryID  string
	namespace string
	sessionID string
}

var _ Backend = (*AgentCoreMemory)(nil)

// NewAgentCoreMemory resolves credentials, builds the AWS client, and
// returns a ready AgentCoreMemory. Every failure here must propagate to
// the caller: Billet fails closed on backend construction, it never
// falls back to the memory backend when agentcore-memory was explicitly
// configured (PROPOSAL.md's safety posture).
func NewAgentCoreMemory(ctx context.Context, cfg AgentCoreMemoryConfig, resolver secret.Resolver) (*AgentCoreMemory, error) {
	if cfg.Region == "" {
		return nil, errors.New("agentcore-memory: region is required")
	}
	if cfg.MemoryID == "" {
		return nil, errors.New("agentcore-memory: memoryId is required")
	}
	if cfg.Namespace == "" {
		return nil, errors.New("agentcore-memory: namespace is required")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.CredentialsRef != "" {
		if resolver == nil {
			return nil, errors.New("agentcore-memory: credentialsRef is set but no secret resolver was provided")
		}
		profile, err := resolver.Resolve(ctx, cfg.CredentialsRef)
		if err != nil {
			return nil, fmt.Errorf("agentcore-memory: resolve credentialsRef: %w", err)
		}
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(profile))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("agentcore-memory: load AWS config: %w", err)
	}

	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID, err = randomHexID("sess")
		if err != nil {
			return nil, fmt.Errorf("agentcore-memory: generate session id: %w", err)
		}
	}

	return &AgentCoreMemory{
		api:       bedrockagentcore.NewFromConfig(awsCfg),
		memoryID:  cfg.MemoryID,
		namespace: cfg.Namespace,
		sessionID: sessionID,
	}, nil
}

// eventPayload is the JSON payload shape stored via CreateEvent. AgentCore
// extracts JSON payloads into long-term memory the same as conversational
// ones; JSON is used instead of the conversational payload type because
// SaveRequest carries no speaker role for Billet to invent one. Field
// names on the wire come from the `document` struct tag, the convention
// document.Marshaler uses (it does not read `json` tags).
type eventPayload struct {
	Content string `document:"content"`
	Kind    string `document:"kind"`
}

// Save implements Backend by calling CreateEvent.
func (a *AgentCoreMemory) Save(ctx context.Context, req SaveRequest) (string, error) {
	kind := req.Kind
	if kind == "" {
		kind = KindEvent
	}

	out, err := a.api.CreateEvent(ctx, &bedrockagentcore.CreateEventInput{
		MemoryId:       aws.String(a.memoryID),
		ActorId:        aws.String(a.namespace),
		SessionId:      aws.String(a.sessionID),
		EventTimestamp: aws.Time(time.Now().UTC()),
		Payload: []types.PayloadType{
			&types.PayloadTypeMemberJson{
				Value: types.MemoryJsonData{
					Content: document.NewLazyDocument(eventPayload{Content: req.Content, Kind: string(kind)}),
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("agentcore-memory: CreateEvent: %w", err)
	}
	if out.Event == nil || out.Event.EventId == nil {
		return "", errors.New("agentcore-memory: CreateEvent returned no event id")
	}
	return aws.ToString(out.Event.EventId), nil
}

// Search implements Backend by calling RetrieveMemoryRecords.
func (a *AgentCoreMemory) Search(ctx context.Context, req SearchRequest) ([]Record, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 5
	}
	topK := int32(limit)

	out, err := a.api.RetrieveMemoryRecords(ctx, &bedrockagentcore.RetrieveMemoryRecordsInput{
		MemoryId:   aws.String(a.memoryID),
		Namespace:  aws.String(a.namespace),
		MaxResults: aws.Int32(topK),
		SearchCriteria: &types.SearchCriteria{
			SearchQuery: aws.String(req.Query),
			TopK:        aws.Int32(topK),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("agentcore-memory: RetrieveMemoryRecords: %w", err)
	}

	records := make([]Record, 0, len(out.MemoryRecordSummaries))
	for _, r := range out.MemoryRecordSummaries {
		rec := Record{MemoryID: aws.ToString(r.MemoryRecordId)}
		if text, ok := r.Content.(*types.MemoryContentMemberText); ok {
			rec.Content = text.Value
		}
		if r.Score != nil {
			rec.Score = *r.Score
		}
		if r.CreatedAt != nil {
			rec.CreatedAt = r.CreatedAt.UTC().Format(time.RFC3339)
		}
		records = append(records, rec)
	}
	return records, nil
}
