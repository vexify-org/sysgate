// Package gateway exposes the sysgate HTTP API.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vexify-org/sysgate/internal/pool"
	"github.com/vexify-org/sysgate/internal/sandbox"
)

type Config struct {
	DefaultTTL   time.Duration
	MaxSandboxes int
	ExecTimeout  time.Duration
	ReapInterval time.Duration
}

type Server struct {
	driver  sandbox.Driver
	pool    *pool.Pool
	cfg     Config
	mux     *http.ServeMux
	started time.Time

	mu     sync.RWMutex
	active map[string]*sandbox.Handle

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	created   atomic.Uint64
	destroyed atomic.Uint64
	execs     atomic.Uint64
	errors    atomic.Uint64
}

func New(driver sandbox.Driver, p *pool.Pool, cfg Config) *Server {
	if cfg.DefaultTTL < 0 {
		cfg.DefaultTTL = 0
	}
	if cfg.MaxSandboxes <= 0 {
		cfg.MaxSandboxes = 256
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = 30 * time.Second
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = time.Second
	}
	s := &Server{
		driver:  driver,
		pool:    p,
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		active:  make(map[string]*sandbox.Handle),
		done:    make(chan struct{}),
	}
	s.routes()
	s.wg.Add(1)
	go s.reaper()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /v1/sandboxes", s.handleList)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}", s.handleGet)
	s.mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleDelete)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.handleExec)
}

type createRequest struct {
	TTLSeconds float64           `json:"ttl_seconds"`
	Labels     map[string]string `json:"labels"`
}

type sandboxView struct {
	ID        string            `json:"id"`
	Driver    string            `json:"driver"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type execRequest struct {
	Argv      []string `json:"argv"`
	Env       []string `json:"env,omitempty"`
	WorkDir   string   `json:"workdir,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

type execResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.mu.RLock()
	n := len(s.active)
	s.mu.RUnlock()
	if n >= s.cfg.MaxSandboxes {
		s.fail(w, http.StatusTooManyRequests, errors.New("sandbox limit reached"))
		return
	}

	h, err := s.pool.Acquire(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err)
		return
	}

	now := time.Now()
	h.CreatedAt = now
	ttl := s.cfg.DefaultTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds * float64(time.Second))
	}
	if ttl > 0 {
		h.ExpiresAt = now.Add(ttl)
	} else {
		h.ExpiresAt = time.Time{}
	}
	if req.Labels != nil {
		h.Labels = req.Labels
	}

	s.mu.Lock()
	s.active[h.ID] = h
	s.mu.Unlock()
	s.created.Add(1)
	writeJSON(w, http.StatusCreated, s.view(h))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	out := make([]sandboxView, 0, len(s.active))
	for _, h := range s.active {
		out = append(out, s.view(h))
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.RLock()
	h, ok := s.active[id]
	s.mu.RUnlock()
	if !ok {
		s.fail(w, http.StatusNotFound, sandbox.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.view(h))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, ok := s.active[id]
	if ok {
		delete(s.active, id)
	}
	s.mu.Unlock()
	if !ok {
		s.fail(w, http.StatusNotFound, sandbox.ErrNotFound)
		return
	}
	if err := s.pool.Destroy(r.Context(), id); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		s.errors.Add(1)
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	s.destroyed.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.RLock()
	_, ok := s.active[id]
	s.mu.RUnlock()
	if !ok {
		s.fail(w, http.StatusNotFound, sandbox.ErrNotFound)
		return
	}

	var req execRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Argv) == 0 {
		s.fail(w, http.StatusBadRequest, errors.New("argv is required"))
		return
	}
	timeout := s.cfg.ExecTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	res, err := s.driver.Exec(ctx, id, sandbox.ExecRequest{
		Argv:    req.Argv,
		Env:     req.Env,
		WorkDir: req.WorkDir,
		Timeout: timeout,
	})
	if err != nil {
		s.errors.Add(1)
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			s.fail(w, http.StatusGatewayTimeout, errors.New("exec timed out"))
		case errors.Is(err, sandbox.ErrNotFound):
			s.fail(w, http.StatusNotFound, err)
		default:
			s.fail(w, http.StatusBadGateway, err)
		}
		return
	}
	s.execs.Add(1)
	writeJSON(w, http.StatusOK, execResponse{
		Stdout:     string(res.Stdout),
		Stderr:     string(res.Stderr),
		ExitCode:   res.ExitCode,
		DurationMS: time.Since(start).Milliseconds(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "driver": s.driver.Name()})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ps := s.pool.Stats()
	s.mu.RLock()
	active := len(s.active)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, `# HELP sysgate_sandboxes_created_total Total sandboxes handed out.
# TYPE sysgate_sandboxes_created_total counter
sysgate_sandboxes_created_total %d
# HELP sysgate_sandboxes_destroyed_total Total sandboxes destroyed.
# TYPE sysgate_sandboxes_destroyed_total counter
sysgate_sandboxes_destroyed_total %d
# HELP sysgate_exec_total Total exec requests served.
# TYPE sysgate_exec_total counter
sysgate_exec_total %d
# HELP sysgate_errors_total Total internal errors.
# TYPE sysgate_errors_total counter
sysgate_errors_total %d
# HELP sysgate_active_sandboxes Currently active sandboxes.
# TYPE sysgate_active_sandboxes gauge
sysgate_active_sandboxes %d
# HELP sysgate_pool_warm Currently warm sandboxes.
# TYPE sysgate_pool_warm gauge
sysgate_pool_warm %d
# HELP sysgate_pool_hits_total Warm pool hits.
# TYPE sysgate_pool_hits_total counter
sysgate_pool_hits_total %d
# HELP sysgate_pool_misses_total Warm pool misses.
# TYPE sysgate_pool_misses_total counter
sysgate_pool_misses_total %d
# HELP sysgate_uptime_seconds Process uptime.
# TYPE sysgate_uptime_seconds gauge
sysgate_uptime_seconds %.0f
`, s.created.Load(), s.destroyed.Load(), s.execs.Load(), s.errors.Load(),
		active, ps.Warm, ps.Hits, ps.Misses, time.Since(s.started).Seconds())
}

func (s *Server) reaper() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			s.reapOnce(now)
		}
	}
}

func (s *Server) reapOnce(now time.Time) {
	var expired []string
	s.mu.Lock()
	for id, h := range s.active {
		if !h.ExpiresAt.IsZero() && now.After(h.ExpiresAt) {
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		delete(s.active, id)
	}
	s.mu.Unlock()
	for _, id := range expired {
		if err := s.pool.Destroy(context.Background(), id); err != nil {
			s.errors.Add(1)
			slog.Warn("reap destroy failed", "id", id, "err", err)
		}
		s.destroyed.Add(1)
	}
}

// Close stops the TTL reaper. It does not close the pool or driver.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
	})
}

func (s *Server) view(h *sandbox.Handle) sandboxView {
	v := sandboxView{ID: h.ID, Driver: h.Driver, CreatedAt: h.CreatedAt, Labels: h.Labels}
	if !h.ExpiresAt.IsZero() {
		t := h.ExpiresAt
		v.ExpiresAt = &t
	}
	return v
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	if code >= 500 {
		slog.Warn("request failed", "status", code, "err", err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}
