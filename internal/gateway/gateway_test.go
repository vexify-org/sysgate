package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vexify-org/sysgate/internal/gateway"
	"github.com/vexify-org/sysgate/internal/pool"
	"github.com/vexify-org/sysgate/internal/sandbox"
	"github.com/vexify-org/sysgate/internal/sandbox/fake"
)

func newServer(t *testing.T, cfg gateway.Config) *httptest.Server {
	t.Helper()
	d := fake.New()
	p := pool.New(d, sandbox.Spec{}, 1)
	s := gateway.New(d, p, cfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.Close()
		p.Close()
		d.Close()
	})
	return ts
}

func doJSON(t *testing.T, method, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestLifecycle(t *testing.T) {
	ts := newServer(t, gateway.Config{DefaultTTL: time.Minute, ExecTimeout: 5 * time.Second})

	resp, body := doJSON(t, "POST", ts.URL+"/v1/sandboxes", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("missing id")
	}

	resp, body = doJSON(t, "POST", ts.URL+"/v1/sandboxes/"+id+"/exec", map[string]any{"argv": []string{"echo", "hi"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec: %d %v", resp.StatusCode, body)
	}
	if body["stdout"] != "fake:echo hi" {
		t.Fatalf("stdout = %v", body["stdout"])
	}

	resp, _ = doJSON(t, "DELETE", ts.URL+"/v1/sandboxes/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", ts.URL+"/v1/sandboxes/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: %d", resp.StatusCode)
	}
}

func TestCreateEmptyBody(t *testing.T) {
	ts := newServer(t, gateway.Config{})

	req, err := http.NewRequest("POST", ts.URL+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("empty-body create: %d, want 201", resp.StatusCode)
	}
}

func TestListAndLimit(t *testing.T) {
	ts := newServer(t, gateway.Config{MaxSandboxes: 1})

	resp, _ := doJSON(t, "POST", ts.URL+"/v1/sandboxes", map[string]any{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "POST", ts.URL+"/v1/sandboxes", map[string]any{})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second create: %d, want 429", resp.StatusCode)
	}

	resp, body := doJSON(t, "GET", ts.URL+"/v1/sandboxes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	_ = body
}

func TestExecValidation(t *testing.T) {
	ts := newServer(t, gateway.Config{})

	_, body := doJSON(t, "POST", ts.URL+"/v1/sandboxes", map[string]any{})
	id, _ := body["id"].(string)

	resp, _ := doJSON(t, "POST", ts.URL+"/v1/sandboxes/"+id+"/exec", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty argv: %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "POST", ts.URL+"/v1/sandboxes/nope/exec", map[string]any{"argv": []string{"true"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing sandbox: %d", resp.StatusCode)
	}
}

func TestHealthAndMetrics(t *testing.T) {
	ts := newServer(t, gateway.Config{})

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %v", err, resp)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/metrics")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: %v %v", err, resp)
	}
	resp.Body.Close()
}

func TestReapExpired(t *testing.T) {
	ts := newServer(t, gateway.Config{ReapInterval: 10 * time.Millisecond})

	_, body := doJSON(t, "POST", ts.URL+"/v1/sandboxes", map[string]any{"ttl_seconds": 0.03})
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("missing id")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := doJSON(t, "GET", ts.URL+"/v1/sandboxes/"+id, nil)
		if resp.StatusCode == http.StatusNotFound {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sandbox was not reaped")
}
