package pool_test

import (
	"context"
	"testing"
	"time"

	"github.com/vexify-org/sysgate/internal/pool"
	"github.com/vexify-org/sysgate/internal/sandbox"
	"github.com/vexify-org/sysgate/internal/sandbox/fake"
)

func waitWarm(t *testing.T, p *pool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Warm >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pool did not warm to %d (got %d)", n, p.Stats().Warm)
}

func TestWarmAcquire(t *testing.T) {
	d := fake.New()
	defer d.Close()
	p := pool.New(d, sandbox.Spec{}, 2)
	defer p.Close()

	waitWarm(t, p, 2)
	h1, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h1.ID == h2.ID {
		t.Fatalf("duplicate id %q", h1.ID)
	}
	if got := p.Stats().Hits; got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
	waitWarm(t, p, 2)
}

func TestOnDemand(t *testing.T) {
	d := fake.New()
	defer d.Close()
	p := pool.New(d, sandbox.Spec{}, 0)
	defer p.Close()

	h, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		t.Fatal("nil handle")
	}
	if got := p.Stats().Misses; got != 1 {
		t.Fatalf("misses = %d, want 1", got)
	}
}

func TestAcquireCanceled(t *testing.T) {
	d := fake.New()
	defer d.Close()
	p := pool.New(d, sandbox.Spec{}, 0)
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Acquire(ctx); err == nil {
		t.Fatal("expected error")
	}
}

func TestCloseDestroysWarm(t *testing.T) {
	d := fake.New()
	defer d.Close()
	p := pool.New(d, sandbox.Spec{}, 3)
	waitWarm(t, p, 3)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if got := p.Stats().Warm; got != 0 {
		t.Fatalf("warm after close = %d, want 0", got)
	}
}
