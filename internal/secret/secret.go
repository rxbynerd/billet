// Package secret defines the Resolver seam for secret:// references, the
// suite-wide convention for keeping credentials out of configuration.
// BilletConfig carries references such as secret://AWS_PROFILE, never
// literal values, so a config file or its JSON dump is always safe to
// commit or share.
package secret

import "context"

// Resolver dereferences a secret:// reference to its value. Resolve fails
// on a malformed reference or an absent secret — it never returns an
// empty value to signal failure.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}
