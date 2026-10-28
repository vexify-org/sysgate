// Package runsc drives gVisor sandboxes through the runsc CLI.
package runsc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vexify-org/sysgate/internal/sandbox"
)

const defaultInit = "/bin/sh"

// Driver runs gVisor sandboxes with runsc.
type Driver struct {
	Bin     string // runsc binary
	Root    string // runsc state root (--root)
	Rootfs  string // default root filesystem
	Network string // none | host | sandbox
}

func New(bin, root, rootfs, network string) (*Driver, error) {
	if bin == "" {
		bin = "runsc"
	}
	if root == "" {
		root = "/var/lib/sysgate"
	}
	if network == "" {
		network = "none"
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create state root: %w", err)
	}
	return &Driver{Bin: bin, Root: root, Rootfs: rootfs, Network: network}, nil
}

func (d *Driver) Name() string { return "runsc" }

func (d *Driver) Create(ctx context.Context, spec sandbox.Spec) (*sandbox.Handle, error) {
	rootfs := spec.Rootfs
	if rootfs == "" {
		rootfs = d.Rootfs
	}
	if rootfs == "" {
		return nil, errors.New("runsc: rootfs is required (set SYSGATE_ROOTFS or Spec.Rootfs)")
	}
	if st, err := os.Stat(rootfs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("runsc: rootfs %q is not a directory", rootfs)
	}

	id := newID()
	dir := filepath.Join(d.Root, "sandboxes", id)
	bundle := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		return nil, fmt.Errorf("create bundle: %w", err)
	}

	initArgs := spec.Init
	if len(initArgs) == 0 {
		initArgs = []string{defaultInit, "-c", "while :; do sleep 3600; done"}
	}
	cfg := buildOCI(id, rootfs, initArgs, spec.Env)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644); err != nil {
		return nil, fmt.Errorf("write config.json: %w", err)
	}

	out, err := d.run(ctx, "--root", d.Root, "run", "-d", "--bundle", bundle, "--network", d.Network, id)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("runsc run: %w: %s", err, strings.TrimSpace(out))
	}

	now := time.Now()
	h := &sandbox.Handle{ID: id, Driver: d.Name(), CreatedAt: now, Labels: spec.Labels}
	if spec.TTL > 0 {
		h.ExpiresAt = now.Add(spec.TTL)
	}
	return h, nil
}

func (d *Driver) Exec(ctx context.Context, id string, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if !validID(id) {
		return nil, fmt.Errorf("runsc: invalid sandbox id %q", id)
	}
	if len(req.Argv) == 0 {
		return nil, errors.New("runsc: argv is required")
	}
	args := []string{"--root", d.Root, "exec"}
	if req.WorkDir != "" {
		args = append(args, "--cwd", req.WorkDir)
	}
	for _, e := range req.Env {
		args = append(args, "--env", e)
	}
	args = append(args, id)
	args = append(args, req.Argv...)

	cmd := exec.CommandContext(ctx, d.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			code = ee.ExitCode()
		} else {
			return nil, err
		}
	}
	return &sandbox.ExecResult{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: code,
		Duration: dur,
	}, nil
}

func (d *Driver) Destroy(ctx context.Context, id string) error {
	if !validID(id) {
		return fmt.Errorf("runsc: invalid sandbox id %q", id)
	}
	_, _ = d.run(ctx, "--root", d.Root, "kill", id, "KILL")
	out, err := d.run(ctx, "--root", d.Root, "delete", "-force", id)
	_ = os.RemoveAll(filepath.Join(d.Root, "sandboxes", id))
	if err != nil && !strings.Contains(out, "not found") && !strings.Contains(out, "does not exist") {
		return fmt.Errorf("runsc delete: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (d *Driver) Close() error { return nil }

func (d *Driver) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sg-%d", time.Now().UnixNano())
	}
	return "sg-" + hex.EncodeToString(b)
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

type ociConfig struct {
	OCIVersion string     `json:"ociVersion"`
	Process    ociProcess `json:"process"`
	Root       ociRoot    `json:"root"`
	Hostname   string     `json:"hostname"`
	Mounts     []ociMount `json:"mounts"`
	Linux      ociLinux   `json:"linux"`
}

type ociProcess struct {
	Terminal bool     `json:"terminal"`
	User     ociUser  `json:"user"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
}

type ociUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces []ociNamespace `json:"namespaces"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

func buildOCI(id, rootfs string, initArgs, extraEnv []string) ociConfig {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOSTNAME=" + id,
	}
	env = append(env, extraEnv...)
	return ociConfig{
		OCIVersion: "1.0.2",
		Process: ociProcess{
			Terminal: false,
			User:     ociUser{UID: 0, GID: 0},
			Args:     initArgs,
			Env:      env,
			Cwd:      "/",
		},
		Root:     ociRoot{Path: rootfs},
		Hostname: id,
		Mounts: []ociMount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=64m"}},
		},
		Linux: ociLinux{Namespaces: []ociNamespace{
			{Type: "pid"},
			{Type: "network"},
			{Type: "ipc"},
			{Type: "uts"},
			{Type: "mount"},
		}},
	}
}
