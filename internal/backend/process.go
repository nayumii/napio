package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Process runs the Factorio server as a direct child process.
//
// It exists mainly to prove that the Backend abstraction is not Docker-shaped;
// Docker remains the first-class deployment. Note that a child process does
// not survive a napio restart the way a container does: if napio is restarted
// while the server runs, this backend loses track of it. Prefer Docker (or a
// systemd unit, once that backend exists) for unattended deployments.
type Process struct {
	command []string
	dir     string
	env     []string
	log     *slog.Logger

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

// NewProcess builds a process backend from an argv slice.
func NewProcess(command []string, dir string, env []string, log *slog.Logger) *Process {
	return &Process{command: command, dir: dir, env: env, log: log}
}

func (p *Process) Describe() string {
	if len(p.command) == 0 {
		return "process (no command)"
	}
	return "process " + p.command[0]
}

func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil && !p.exited() {
		return nil
	}
	if len(p.command) == 0 {
		return errors.New("process backend: no command configured")
	}

	cmd := exec.Command(p.command[0], p.command[1:]...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), p.env...)
	// Put the child in its own process group so a signal reaches the whole
	// tree, including wrapper scripts.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("process backend: starting %s: %w", p.command[0], err)
	}

	done := make(chan struct{})
	p.cmd, p.done = cmd, done
	go func() {
		err := cmd.Wait()
		close(done)
		if p.log != nil {
			p.log.Info("factorio process exited", "error", err)
		}
	}()
	return nil
}

func (p *Process) Stop(ctx context.Context, timeout time.Duration) error {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// SIGTERM makes the Factorio headless server save and exit cleanly
	// ("Quitting: signal." followed by "Saving map as ...").
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
	case <-ctx.Done():
		return ctx.Err()
	}

	if p.log != nil {
		p.log.Warn("factorio did not exit after SIGTERM, sending SIGKILL; unsaved progress may be lost",
			"timeout", timeout)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("process backend: process survived SIGKILL")
	}
}

func (p *Process) IsRunning(ctx context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return false, nil
	}
	return !p.exited(), nil
}

// exited must be called with the mutex held.
func (p *Process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
