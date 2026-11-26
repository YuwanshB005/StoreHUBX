package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A component build runs `npm ci && npm run build` on code written by any
// user. That code must never see the worker's environment (database URL,
// storage keys, the key that decrypts users' GitHub tokens) or its files.
//
// BUILD_SANDBOX=docker (default) runs it in a throwaway container:
//   - the source is copied in with `docker cp` (no host bind mount)
//   - no environment variables from the worker are passed
//   - CPU, memory, process-count and wall-clock limits
//   - all Linux capabilities dropped, no privilege escalation
//   - only dist/ or build/ is copied back out, and output containing
//     symlinks is rejected (a symlink could point at a worker file)
//
// BUILD_SANDBOX=host runs npm directly on the worker with a scrubbed
// environment. It is only for local experiments without Docker.
type sandboxConfig struct {
	Mode    string
	Image   string
	Memory  string
	CPUs    string
	Pids    string
	Network string
	Timeout time.Duration
}

func loadSandboxConfig() sandboxConfig {
	c := sandboxConfig{
		Mode:    envOr("BUILD_SANDBOX", "docker"),
		Image:   envOr("BUILD_IMAGE", "node:22-bookworm-slim"),
		Memory:  envOr("BUILD_MEMORY", "2g"),
		CPUs:    envOr("BUILD_CPUS", "2"),
		Pids:    envOr("BUILD_PIDS_LIMIT", "512"),
		Network: envOr("BUILD_NETWORK", "bridge"), // npm ci needs the registry
		Timeout: 10 * time.Minute,
	}
	if d, err := time.ParseDuration(os.Getenv("BUILD_TIMEOUT")); err == nil && d > 0 {
		c.Timeout = d
	}
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const buildScript = "npm ci --no-audit --no-fund && npm run build"

// errBuildFailed marks a build that ran and failed (bad code, failing
// script, limit exceeded). Retrying it would fail the same way.
var errBuildFailed = errors.New("build failed")

// runNodeBuild builds workingDir in place, leaving dist/ or build/ there.
func runNodeBuild(ctx context.Context, name, workingDir string, logf func(string)) error {
	cfg := loadSandboxConfig()
	switch cfg.Mode {
	case "docker":
		return dockerBuild(ctx, cfg, name, workingDir, logf)
	case "host":
		logf("[WARN] BUILD_SANDBOX=host: running untrusted build without isolation")
		return hostBuild(ctx, cfg, workingDir, logf)
	default:
		return fmt.Errorf("unknown BUILD_SANDBOX %q (use docker or host)", cfg.Mode)
	}
}

func dockerBuild(ctx context.Context, cfg sandboxConfig, name, workingDir string, logf func(string)) error {
	// Container cleanup must run even when ctx is already cancelled.
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(c, "docker", "rm", "-f", name).Run()
	}
	cleanup() // a leftover from a crashed attempt would block the name
	defer cleanup()

	create := []string{"create", "--name", name,
		"--network", cfg.Network,
		"--memory", cfg.Memory, "--memory-swap", cfg.Memory,
		"--cpus", cfg.CPUs,
		"--pids-limit", cfg.Pids,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"-e", "CI=true", "-e", "HOME=/tmp", "-e", "npm_config_update_notifier=false",
		"-w", "/work",
		cfg.Image, "sh", "-c", buildScript,
	}
	if out, err := exec.CommandContext(ctx, "docker", create...).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox create failed (is Docker running and %s available?): %v: %s", cfg.Image, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", workingDir+"/.", name+":/work").CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox copy-in failed: %v: %s", err, strings.TrimSpace(string(out)))
	}

	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	logf(fmt.Sprintf("[sandbox] %s, memory %s, cpus %s, timeout %s", cfg.Image, cfg.Memory, cfg.CPUs, cfg.Timeout))
	err := streamCommand(exec.CommandContext(runCtx, "docker", "start", "-a", name), logf)
	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: timed out after %s", errBuildFailed, cfg.Timeout)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if state := containerState(name); state != "" {
			return fmt.Errorf("%w: %s", errBuildFailed, state)
		}
		return fmt.Errorf("%w: %v", errBuildFailed, err)
	}

	// Copy back only the build output, replacing anything the repo had.
	for _, dir := range []string{"dist", "build"} {
		tmp := filepath.Join(workingDir, ".sandbox-"+dir)
		_ = os.RemoveAll(tmp)
		if exec.CommandContext(ctx, "docker", "cp", name+":/work/"+dir, tmp).Run() != nil {
			continue // this output folder doesn't exist
		}
		dst := filepath.Join(workingDir, dir)
		_ = os.RemoveAll(dst)
		if err := os.Rename(tmp, dst); err != nil {
			return fmt.Errorf("sandbox copy-out failed: %w", err)
		}
	}
	return nil
}

// containerState describes why a container stopped (exit code, OOM kill).
func containerState(name string) string {
	out, err := exec.Command("docker", "inspect", "-f", "exit code {{.State.ExitCode}}{{if .State.OOMKilled}}, killed: out of memory{{end}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func hostBuild(ctx context.Context, cfg sandboxConfig, workingDir string, logf func(string)) error {
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", buildScript)
	cmd.Dir = workingDir
	// Never inherit the worker's environment (database URL, storage keys...).
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + workingDir, "CI=true"}
	if err := streamCommand(cmd, logf); err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%w: timed out after %s", errBuildFailed, cfg.Timeout)
		}
		return fmt.Errorf("%w: %v", errBuildFailed, err)
	}
	return nil
}

// streamCommand runs cmd, sending each stdout/stderr line to logf.
func streamCommand(cmd *exec.Cmd, logf func(string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex // logf is called from two goroutines; keep lines whole and ordered
	pump := func(r io.Reader, prefix string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			mu.Lock()
			logf(prefix + sc.Text())
			mu.Unlock()
		}
	}
	wg.Add(2)
	go pump(stdout, "[build] ")
	go pump(stderr, "[build ERROR] ")
	wg.Wait()
	return cmd.Wait()
}

// checkNoSymlinks rejects build output containing symlinks. The worker reads
// and uploads these files on the host, so a symlink such as
// dist/x -> /proc/self/environ would publish a worker file.
func checkNoSymlinks(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: output folder is a symlink", errBuildFailed)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("%w: output contains a symlink (%s), which is not allowed", errBuildFailed, rel)
		}
		return nil
	})
}
