package runsc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vexify-org/sysgate/internal/sandbox"
)

func TestBuildOCIConfig(t *testing.T) {
	cfg := buildOCI("sg-abc", "/srv/rootfs", []string{"/bin/sh", "-c", "true"}, []string{"FOO=bar"})
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["ociVersion"] != "1.0.2" {
		t.Fatalf("ociVersion = %v", got["ociVersion"])
	}
	proc := got["process"].(map[string]any)
	args := proc["args"].([]any)
	if len(args) != 3 || args[0] != "/bin/sh" {
		t.Fatalf("args = %v", args)
	}
	root := got["root"].(map[string]any)
	if root["path"] != "/srv/rootfs" {
		t.Fatalf("root.path = %v", root["path"])
	}
	found := false
	for _, e := range proc["env"].([]any) {
		if e == "FOO=bar" {
			found = true
		}
	}
	if !found {
		t.Fatal("extra env missing")
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"sg-abc123", "abc", "a-b-c"} {
		if !validID(id) {
			t.Errorf("validID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "../etc", "SG-ABC", "a/b", "a b"} {
		if validID(id) {
			t.Errorf("validID(%q) = true", id)
		}
	}
}

func TestCreateRequiresRootfs(t *testing.T) {
	d, err := New("runsc", t.TempDir(), "", "none")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(context.Background(), sandbox.Spec{}); err == nil || !strings.Contains(err.Error(), "rootfs") {
		t.Fatalf("err = %v, want rootfs error", err)
	}
}

func TestExecRejectsBadID(t *testing.T) {
	d, err := New("runsc", t.TempDir(), "", "none")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(context.Background(), "../etc", sandbox.ExecRequest{Argv: []string{"true"}}); err == nil {
		t.Fatal("expected invalid id error")
	}
}
