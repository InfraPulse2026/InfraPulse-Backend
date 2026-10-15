package automation

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

//go:embed runner.py
var runnerScript []byte

type Process struct {
	Python          string
	CredentialsDir  string
	WorkDir         string
	AllowedNetworks []netip.Prefix
}
type boundedOutput struct {
	data     bytes.Buffer
	max      int
	exceeded bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.max-w.data.Len() {
		w.exceeded = true
		return n, nil
	}
	_, _ = w.data.Write(p)
	return n, nil
}
func (p Process) invoke(ctx context.Context, input any, max int) ([]byte, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("Ansible control node requires Linux")
	}
	if !filepath.IsAbs(p.Python) || !filepath.IsAbs(p.WorkDir) {
		return nil, ErrInvalid
	}
	dir, err := os.MkdirTemp(p.WorkDir, "infrapulse-job-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "runner.py")
	if err = os.WriteFile(script, runnerScript, 0600); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	cmd := exec.CommandContext(ctx, p.Python, "-I", script)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(raw)
	// Intentionally do not inherit shell/Ansible/Python configuration or secrets.
	cmd.Env = []string{"PATH=" + filepath.Dir(p.Python) + ":/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "LANG=C.UTF-8", "PYTHONUNBUFFERED=1"}
	out := &boundedOutput{max: max}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	prepareProcess(cmd)
	err = cmd.Run()
	if err != nil || out.exceeded {
		return nil, errors.New("bounded runner failed")
	}
	return out.data.Bytes(), nil
}
func (p Process) Probe(ctx context.Context) WorkerStatus {
	raw, err := p.invoke(ctx, map[string]any{"probe": true}, 16384)
	if err != nil {
		return WorkerStatus{ErrorCode: "runner_unavailable"}
	}
	var v WorkerStatus
	if json.Unmarshal(raw, &v) != nil || !v.Available {
		return WorkerStatus{ErrorCode: "runner_unavailable"}
	}
	return v
}

type credential struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	EnablePassword string `json:"enable_password,omitempty"`
}

func (p Process) credentials(ref string) (credential, error) {
	var v credential
	if !Identifier(ref) || !filepath.IsAbs(p.CredentialsDir) {
		return v, ErrInvalid
	}
	// os.Root prevents symlinks from escaping the credential directory.
	root, err := os.OpenRoot(p.CredentialsDir)
	if err != nil {
		return v, err
	}
	defer root.Close()
	f, err := root.Open(ref + ".json")
	if err != nil {
		return v, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return v, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return v, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return v, err
	}
	defer clear(raw)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&v) != nil {
		return v, ErrInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || len(v.Username) == 0 || len(v.Username) > 128 || len(v.Password) == 0 || len(v.Password) > 4096 || len(v.EnablePassword) > 4096 {
		return v, ErrInvalid
	}
	return v, nil
}
func (p Process) Execute(ctx context.Context, j Job) Result {
	if j.Target.Validate() != nil {
		return Result{ErrorCode: "invalid_result"}
	}
	addr, err := netip.ParseAddr(j.Target.Host)
	if err != nil {
		return Result{ErrorCode: "destination_denied"}
	}
	allowed := false
	for _, prefix := range p.AllowedNetworks {
		if prefix.Contains(addr) {
			allowed = true
		}
	}
	if !allowed {
		return Result{ErrorCode: "destination_denied"}
	}
	cred, err := p.credentials(j.Target.CredentialRef)
	if err != nil {
		return Result{ErrorCode: "credential_unavailable"}
	}
	in := map[string]any{"target": j.Target, "credential": cred, "timeout_seconds": j.TimeoutSeconds}
	raw, err := p.invoke(ctx, in, (MaxConfiguration+MaxFacts)*6+4096)
	if err != nil {
		return Result{ErrorCode: "runner_failed"}
	}
	defer clear(raw)
	var result Result
	if json.Unmarshal(raw, &result) != nil {
		return Result{ErrorCode: "invalid_result"}
	}
	return result
}
