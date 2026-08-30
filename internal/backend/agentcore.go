package backend

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
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

// regionShapePattern is a cheap sanity check on an AWS region string: two
// or more lowercase alphanumeric segments joined by hyphens (matching
// every standard, GovCloud, and China partition region observed in
// practice, e.g. eu-west-2, us-gov-west-1, cn-north-1). It catches an
// obvious typo (whitespace, a credential pasted into the region field, an
// empty string) before any network call; it does not — and cannot,
// without calling AWS — confirm the region actually exists (TODO.md).
var regionShapePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)+$`)

func validRegionShape(region string) bool {
	return regionShapePattern.MatchString(region)
}

// namespaceDelimiter separates a bound namespace from anything that might
// follow it, so one namespace can never be mistaken for a prefix of
// another under RetrieveMemoryRecords' prefix-match semantics (see
// boundNamespace).
const namespaceDelimiter = "/"

// boundNamespace returns ns, trimmed, with namespaceDelimiter appended.
// AgentCore's RetrieveMemoryRecords documents its Namespace field as a
// *prefix* filter, not an exact match — so an unqualified namespace like
// "acme" would also match records under "acme-corp" or "acme2". Appending
// a delimiter before using the value as either AgentCore's actorId (on
// write, via CreateEvent) or its namespace prefix (on read, via
// RetrieveMemoryRecords) closes that gap: "acme/" cannot prefix-match
// "acme-corp/". Both call sites bind the same boundNamespace value, so
// reads and writes stay coherent.
func boundNamespace(ns string) string {
	return strings.TrimSpace(ns) + namespaceDelimiter
}

// probeCredentials resolves provider once so an absent or invalid AWS
// credential chain fails backend construction immediately, rather than
// being discovered silently on the first save_memory/search_memory call.
func probeCredentials(ctx context.Context, provider aws.CredentialsProvider) error {
	if provider == nil {
		return errors.New("agentcore-memory: no AWS credentials provider configured")
	}
	if _, err := provider.Retrieve(ctx); err != nil {
		return fmt.Errorf("agentcore-memory: resolve AWS credentials: %w", err)
	}
	return nil
}

// AgentCoreMemoryConfig configures the AgentCoreMemory backend.
type AgentCoreMemoryConfig struct {
	// Region is the AWS region hosting the AgentCore Memory resource.
	Region string
	// MemoryID identifies the AgentCore Memory resource.
	MemoryID string
	// Namespace is Billet's namespace. boundNamespace(Namespace) is bound
	// to AgentCore's actorId on every CreateEvent and used as the
	// RetrieveMemoryRecords namespace filter (docs/DECISIONS.md).
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
	if !validRegionShape(cfg.Region) {
		return nil, fmt.Errorf("agentcore-memory: region %q is not shaped like an AWS region", cfg.Region)
	}
	if cfg.MemoryID == "" {
		return nil, errors.New("agentcore-memory: memoryId is required")
	}
	if strings.TrimSpace(cfg.Namespace) == "" {
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
		// A resolved credentialsRef that names a profile the shared config
		// files don't have would otherwise propagate that value (the SDK's
		// own error embeds it) straight into the startup log.
		var profileNotExist awsconfig.SharedConfigProfileNotExistError
		if errors.As(err, &profileNotExist) {
			return nil, errors.New("agentcore-memory: the configured AWS shared-config profile does not exist")
		}
		return nil, fmt.Errorf("agentcore-memory: load AWS config: %w", err)
	}

	if err := probeCredentials(ctx, awsCfg.Credentials); err != nil {
		return nil, err
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
		namespace: boundNamespace(cfg.Namespace),
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

// maxSearchResults bounds the value Search converts to AWS's int32
// MaxResults/TopK fields: without a ceiling, a Limit near or above
// math.MaxInt32 would silently wrap to a negative value on conversion.
// The MCP tool layer already clamps to this range (internal/mcpserver);
// this is a second, backend-local guard for any other caller of Backend.
const maxSearchResults = 100

// Search implements Backend by calling RetrieveMemoryRecords.
func (a *AgentCoreMemory) Search(ctx context.Context, req SearchRequest) ([]Record, error) {
	limit := req.Limit
	switch {
	case limit <= 0:
		limit = 5
	case limit > maxSearchResults:
		limit = maxSearchResults
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
