// Package pool keeps pre-created sandboxes warm so gateway requests can be
// served in microseconds instead of paying runsc boot latency.
package pool

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vexify-org/sysgate/internal/sandbox"
)

type Stats struct {
	Warm    int
	Hits    uint64
	Misses  uint64
	Created uint64
	Errors  uint64
}

type Pool struct {
	driver sandbox.Driver
	spec   sandbox.Spec
	size   int
	warm   chan *sandbox.Handle
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup

	hits    atomic.Uint64
	misses  atomic.Uint64
	created atomic.Uint64
	errors  atomic.Uint64
}

// New starts a pool of the given size. Size 0 disables warming; Acquire then
// creates sandboxes on demand.
func New(driver sandbox.Driver, spec sandbox.Spec, size int) *Pool {
	if size < 0 {
		size = 0
	}
	p := &Pool{
		driver: driver,
		spec:   spec,
		size:   size,
		warm:   make(chan *sandbox.Handle, size),
		done:   make(chan struct{}),
	}
	if size > 0 {
		p.wg.Add(1)
		go p.refill()
	}
	return p
}

func (p *Pool) refill() {
	defer p.wg.Done()
	backoff := 100 * time.Millisecond
	for {
		select {
		case <-p.done:
			return
		default:
		}
		h, err := p.driver.Create(context.Background(), p.spec)
		if err != nil {
			p.errors.Add(1)
			slog.Warn("warm pool create failed", "err", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-p.done:
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
			continue
		}
		backoff = 100 * time.Millisecond
		p.created.Add(1)
		select {
		case p.warm <- h:
		case <-p.done:
			_ = p.driver.Destroy(context.Background(), h.ID)
			return
		}
	}
}

// Acquire hands out a warm sandbox if one is ready, otherwise falls back to
// creating one on demand.
func (p *Pool) Acquire(ctx context.Context) (*sandbox.Handle, error) {
	select {
	case h := <-p.warm:
		p.hits.Add(1)
		return h, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	p.misses.Add(1)
	h, err := p.driver.Create(ctx, p.spec)
	if err != nil {
		p.errors.Add(1)
		return nil, err
	}
	p.created.Add(1)
	return h, nil
}

// Destroy tears down a sandbox handed out by Acquire.
func (p *Pool) Destroy(ctx context.Context, id string) error {
	return p.driver.Destroy(ctx, id)
}

func (p *Pool) Stats() Stats {
	return Stats{
		Warm:    len(p.warm),
		Hits:    p.hits.Load(),
		Misses:  p.misses.Load(),
		Created: p.created.Load(),
		Errors:  p.errors.Load(),
	}
}

// Close stops refilling and destroys every warm sandbox.
func (p *Pool) Close() error {
	var err error
	p.once.Do(func() {
		close(p.done)
		p.wg.Wait()
		for {
			select {
			case h := <-p.warm:
				if derr := p.driver.Destroy(context.Background(), h.ID); derr != nil && err == nil {
					err = derr
				}
			default:
				return
			}
		}
	})
	return err
}
