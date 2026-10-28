// Package sandbox defines the driver abstraction sysgate uses to run isolated
// workloads: gVisor via runsc in production, in-memory fakes in tests.
package sandbox

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("sandbox not found")
	ErrClosed   = errors.New("sandbox driver closed")
)

// Spec describes a sandbox to create.
type Spec struct {
	Rootfs string            // root filesystem path (runsc driver)
	Init   []string          // init process argv (driver specific)
	Env    []string          // extra environment variables
	Labels map[string]string // caller metadata
	TTL    time.Duration     // 0 means no expiry
}

// Handle is a created sandbox.
type Handle struct {
	ID        string
	Driver    string
	CreatedAt time.Time
	ExpiresAt time.Time
	Labels    map[string]string
}

// ExecRequest is a command to run inside a sandbox.
type ExecRequest struct {
	Argv    []string
	Env     []string
	WorkDir string
	Timeout time.Duration
}

// ExecResult is the outcome of an ExecRequest.
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
}

// Driver creates, executes in, and destroys sandboxes.
type Driver interface {
	Name() string
	Create(ctx context.Context, spec Spec) (*Handle, error)
	Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)
	Destroy(ctx context.Context, id string) error
	Close() error
}
