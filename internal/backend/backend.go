// Package backend abstracts "how do I start and stop this Factorio server".
//
// The interface is deliberately dumb: start it, stop it, is it running. It
// knows nothing about Factorio. Readiness detection and player counting live
// in the factorio package, which is what keeps adding a backend (systemd, a
// remote host, a hypervisor) a small, self-contained job.
package backend

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"git.naknu.li/emily/napio/internal/config"
)

// Backend controls the lifecycle of one Factorio server process.
type Backend interface {
	// Describe returns a short human-readable identity for logs and the API.
	Describe() string
	// Start launches the server. It returns once the process has been
	// launched, not once Factorio is ready.
	Start(ctx context.Context) error
	// Stop asks the process to terminate gracefully (SIGTERM), waiting up to
	// timeout before escalating to SIGKILL.
	Stop(ctx context.Context, timeout time.Duration) error
	// IsRunning reports whether the process currently exists.
	IsRunning(ctx context.Context) (bool, error)
}

// LogSource is an optional capability: a backend that can show the server's
// own log. napio uses it to read the Factorio version banner, which is the
// only trustworthy source for the exact build while the server is stopped.
//
// It is deliberately not part of Backend: backends stay dumb, and one that
// cannot produce logs simply does not implement this.
type LogSource interface {
	RecentLogs(ctx context.Context, lines int) (string, error)
}

// New builds the backend described by the configuration.
func New(cfg config.Backend, log *slog.Logger) (Backend, error) {
	switch cfg.Type {
	case config.BackendDocker:
		return NewDocker(cfg.Container, cfg.Host, log)
	case config.BackendProcess:
		return NewProcess(cfg.Command, cfg.WorkingDir, cfg.Env, log), nil
	default:
		return nil, fmt.Errorf("unknown backend type %q", cfg.Type)
	}
}
