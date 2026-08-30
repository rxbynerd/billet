package secret

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Prefix introduces every secret reference. A value without it is a
// literal and is rejected outright — Billet never accepts a raw secret
// in configuration.
const Prefix = "secret://"

// Backends a reference may name. The first path segment selects the
// backend; a bare single segment defaults to env, so the documented
// secret://AWS_PROFILE form resolves from the environment.
const (
	BackendEnv  = "env"
	BackendFile = "file"
)

// Ref is a parsed secret reference.
type Ref struct {
	// Backend is BackendEnv or BackendFile.
	Backend string
	// Name is the environment variable name (env) or the absolute file
	// path (file).
	Name string
}

// IsRef reports whether s is shaped like a secret reference. It says
// nothing about whether the reference parses or resolves.
func IsRef(s string) bool {
	return strings.HasPrefix(s, Prefix)
}

// ParseRef parses a secret:// reference. The grammar (a suite-wide
// convention, also implemented by Chiron) is:
//
//	secret://NAME            -> environment variable NAME
//	secret://env/NAME        -> environment variable NAME
//	secret://file/<path>     -> contents of the absolute file path; the
//	                            leading slash is implied, so both
//	                            secret://file/etc/billet/key and
//	                            secret://file//etc/billet/key mean
//	                            /etc/billet/key
//
// A value that fails to parse is never echoed into the returned error:
// the offending value may itself be a credential, and errors end up in
// logs.
func ParseRef(ref string) (Ref, error) {
	if !IsRef(ref) {
		return Ref{}, errors.New("secret: not a secret:// reference; literal secrets are not permitted (use secret://NAME, secret://env/NAME, or secret://file/<absolute path>)")
	}
	rest := strings.TrimPrefix(ref, Prefix)
	if rest == "" {
		return Ref{}, errors.New("secret: empty reference")
	}
	backend, name, cut := strings.Cut(rest, "/")
	if !cut {
		// Bare single segment: an environment variable name.
		return Ref{Backend: BackendEnv, Name: rest}, nil
	}
	switch backend {
	case BackendEnv:
		if name == "" {
			return Ref{}, errors.New("secret: empty environment variable name")
		}
		if strings.Contains(name, "/") {
			return Ref{}, fmt.Errorf("secret: invalid environment variable name %q", name)
		}
		return Ref{Backend: BackendEnv, Name: name}, nil
	case BackendFile:
		if strings.TrimLeft(name, "/") == "" {
			return Ref{}, errors.New("secret: empty file path")
		}
		return Ref{Backend: BackendFile, Name: "/" + strings.TrimPrefix(name, "/")}, nil
	default:
		return Ref{}, fmt.Errorf("secret: unknown backend %q (want %s or %s)", backend, BackendEnv, BackendFile)
	}
}

// Env resolves env-backed references from the process environment. An
// unset or empty variable is an error, never an empty value.
type Env struct{}

// Resolve implements Resolver.
func (Env) Resolve(_ context.Context, ref string) (string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	if r.Backend != BackendEnv {
		return "", fmt.Errorf("secret: env resolver cannot resolve backend %q", r.Backend)
	}
	v, ok := os.LookupEnv(r.Name)
	if !ok {
		return "", fmt.Errorf("secret: environment variable %s is not set", r.Name)
	}
	if v == "" {
		return "", fmt.Errorf("secret: environment variable %s is set but empty", r.Name)
	}
	return v, nil
}

// File resolves file-backed references by reading the named file.
// Trailing newlines are trimmed (secrets files conventionally end with
// one); an empty file is an error.
type File struct{}

// Resolve implements Resolver.
func (File) Resolve(_ context.Context, ref string) (string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	if r.Backend != BackendFile {
		return "", fmt.Errorf("secret: file resolver cannot resolve backend %q", r.Backend)
	}
	b, err := os.ReadFile(r.Name)
	if err != nil {
		return "", fmt.Errorf("secret: reading %s: %w", r.Name, err)
	}
	// A warning, not an error: a mis-permissioned but valid key file must
	// not block startup, but the operator should hear about it.
	if info, statErr := os.Stat(r.Name); statErr == nil && info.Mode().Perm()&0o177 != 0 {
		slog.Warn("secret file permissions are too open; tighten to 0600",
			"path", r.Name, "mode", info.Mode().Perm().String())
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", fmt.Errorf("secret: file %s is empty", r.Name)
	}
	return v, nil
}

// Default returns the v1 resolver: env and file references dispatched by
// backend.
func Default() Resolver {
	return defaultResolver{}
}

type defaultResolver struct{}

func (defaultResolver) Resolve(ctx context.Context, ref string) (string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	switch r.Backend {
	case BackendEnv:
		return Env{}.Resolve(ctx, ref)
	case BackendFile:
		return File{}.Resolve(ctx, ref)
	default:
		return "", fmt.Errorf("secret: no resolver for backend %q", r.Backend)
	}
}

var (
	_ Resolver = Env{}
	_ Resolver = File{}
	_ Resolver = defaultResolver{}
)
