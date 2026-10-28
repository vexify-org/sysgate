// Package fake provides an in-memory sandbox driver for tests and local demos.
package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vexify-org/sysgate/internal/sandbox"
)

// Driver keeps sandboxes in memory. Exec returns "fake:<argv joined>" by
// default; set ExecFn to customise behaviour.
type Driver struct {
	mu        sync.Mutex
	seq       atomic.Uint64
	sandboxes map[string]*sandbox.Handle
	closed    bool

	ExecFn func(id string, req sandbox.ExecRequest) (*sandbox.ExecResult, error)
}

func New() *Driver {
	return &Driver{sandboxes: make(map[string]*sandbox.Handle)}
}

func (d *Driver) Name() string { return "fake" }

func (d *Driver) Create(ctx context.Context, spec sandbox.Spec) (*sandbox.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, sandbox.ErrClosed
	}
	id := fmt.Sprintf("fake-%d", d.seq.Add(1))
	now := time.Now()
	h := &sandbox.Handle{ID: id, Driver: d.Name(), CreatedAt: now, Labels: spec.Labels}
	if spec.TTL > 0 {
		h.ExpiresAt = now.Add(spec.TTL)
	}
	d.sandboxes[id] = h
	return h, nil
}

func (d *Driver) Exec(ctx context.Context, id string, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	_, ok := d.sandboxes[id]
	closed := d.closed
	fn := d.ExecFn
	d.mu.Unlock()
	if closed {
		return nil, sandbox.ErrClosed
	}
	if !ok {
		return nil, sandbox.ErrNotFound
	}
	start := time.Now()
	if fn != nil {
		res, err := fn(id, req)
		if res != nil {
			res.Duration = time.Since(start)
		}
		return res, err
	}
	out := "fake:" + strings.Join(req.Argv, " ")
	return &sandbox.ExecResult{Stdout: []byte(out), ExitCode: 0, Duration: time.Since(start)}, nil
}

func (d *Driver) Destroy(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[id]; !ok {
		return sandbox.ErrNotFound
	}
	delete(d.sandboxes, id)
	return nil
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	d.sandboxes = make(map[string]*sandbox.Handle)
	return nil
}
