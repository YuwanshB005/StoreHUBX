package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run real untrusted-style builds inside the Docker sandbox.
// They skip without Docker unless STOREHUBX_REQUIRE_INFRA=1 (CI).
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("STOREHUBX_REQUIRE_INFRA") == "1" {
			t.Fatalf("docker required: %v", err)
		}
		t.Skip("docker not available")
	}
	if exec.Command("docker", "image", "inspect", loadSandboxConfig().Image).Run() != nil {
		if out, err := exec.Command("docker", "pull", loadSandboxConfig().Image).CombinedOutput(); err != nil {
			t.Skipf("cannot pull build image: %v %s", err, out)
		}
	}
}

// fixture writes a dependency-free npm project whose build runs script.
func fixture(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	pkg := `{"name":"fx","version":"1.0.0","scripts":{"build":` + quote(script) + `}}`
	lock := `{"name":"fx","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"fx","version":"1.0.0"}}}`
	must(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(lock), 0o644))
	return dir
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, dir string) (string, error) {
	t.Helper()
	var logs strings.Builder
	name := "storehubx-test-" + strings.ReplaceAll(filepath.Base(dir), "_", "-")
	err := runNodeBuild(context.Background(), name, dir, func(l string) { logs.WriteString(l + "\n") })
	if out, _ := exec.Command("docker", "ps", "-aq", "-f", "name=^"+name+"$").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("sandbox container %s was not removed", name)
	}
	return logs.String(), err
}

func TestSandboxBuildProducesOutput(t *testing.T) {
	requireDocker(t)
	dir := fixture(t, "mkdir -p dist && echo '<html>ok</html>' > dist/index.html")
	logs, err := build(t, dir)
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, logs)
	}
	b, err := os.ReadFile(filepath.Join(dir, "dist", "index.html"))
	if err != nil || !strings.Contains(string(b), "ok") {
		t.Fatalf("dist/index.html not copied back: %v %q", err, b)
	}
}

func TestSandboxBuildSeesNoWorkerSecrets(t *testing.T) {
	requireDocker(t)
	for _, k := range []string{"MONGO_URI", "TOKEN_ENC_KEY", "JWT_SECRET", "AWS_SECRET_ACCESS_KEY", "GITHUB_CLIENT_SECRET"} {
		t.Setenv(k, "LEAKED-"+k)
	}
	// A malicious build script dumps its environment into the published output.
	dir := fixture(t, "mkdir -p dist && env > dist/env.txt && echo '<html></html>' > dist/index.html")
	logs, err := build(t, dir)
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, logs)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "dist", "env.txt"))
	if strings.Contains(string(env), "LEAKED-") {
		t.Fatalf("worker secret visible inside the build sandbox:\n%s", env)
	}
}

func TestSandboxEnforcesTimeout(t *testing.T) {
	requireDocker(t)
	t.Setenv("BUILD_TIMEOUT", "4s")
	dir := fixture(t, "sleep 60")
	start := time.Now()
	_, err := build(t, dir)
	if err == nil || !errors.Is(err, errBuildFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout failure, got %v", err)
	}
	if time.Since(start) > 40*time.Second {
		t.Fatalf("timeout not enforced, took %s", time.Since(start))
	}
}

func TestSandboxEnforcesMemoryLimit(t *testing.T) {
	requireDocker(t)
	t.Setenv("BUILD_MEMORY", "128m")
	// Allocate and touch ~512 MB.
	dir := fixture(t, "node -e 'const a=[];for(let i=0;i<64;i++){a.push(Buffer.alloc(8*1024*1024,1))}'")
	_, err := build(t, dir)
	if err == nil || !errors.Is(err, errBuildFailed) {
		t.Fatalf("expected the build to be killed by the memory limit, got %v", err)
	}
	t.Logf("memory-limited build stopped: %v", err)
}

func TestOutputWithSymlinkIsRejected(t *testing.T) {
	requireDocker(t)
	// The build plants a symlink pointing at a host path. If the worker
	// followed it while uploading, it would publish that host file.
	dir := fixture(t, "mkdir -p dist && echo '<html></html>' > dist/index.html && ln -s /etc/passwd dist/leak.txt")
	if _, err := build(t, dir); err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if err := checkNoSymlinks(filepath.Join(dir, "dist")); err == nil {
		t.Fatal("symlink in build output was accepted")
	}
}

func TestBuildFailureIsReported(t *testing.T) {
	requireDocker(t)
	dir := fixture(t, "echo boom >&2 && exit 3")
	logs, err := build(t, dir)
	if err == nil || !errors.Is(err, errBuildFailed) || !strings.Contains(err.Error(), "exit code") {
		t.Fatalf("expected a build failure with exit code, got %v", err)
	}
	if !strings.Contains(logs, "boom") {
		t.Fatalf("build stderr not captured in logs:\n%s", logs)
	}
}

func TestHostModeScrubsEnvironment(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	t.Setenv("BUILD_SANDBOX", "host")
	t.Setenv("MONGO_URI", "LEAKED-MONGO")
	dir := fixture(t, "mkdir -p dist && env > dist/env.txt")
	var logs strings.Builder
	if err := runNodeBuild(context.Background(), "unused", dir, func(l string) { logs.WriteString(l) }); err != nil {
		t.Fatalf("host build failed: %v %s", err, logs.String())
	}
	env, _ := os.ReadFile(filepath.Join(dir, "dist", "env.txt"))
	if strings.Contains(string(env), "LEAKED-") {
		t.Fatal("host mode leaked worker environment")
	}
}
