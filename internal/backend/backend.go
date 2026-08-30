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

// ConfigChecker is an optional capability: a backend that can inspect its own
// deployment and report mistakes that would fight napio for control of the
// server. The results are warnings, never hard failures.
type ConfigChecker interface {
	CheckConfiguration(ctx context.Context) []string
}

// GracefulStopper is an optional capability: a backend whose own Stop already
// performs Factorio's clean quit, and which therefore must not be bypassed by
// an out-of-band RCON /quit.
//
// Wings implements it. Its stop command for the Factorio egg is "/quit" on the
// server console, and it skips its crash detection only for a process it
// stopped itself — so a server quit over RCON looks like a crash to Wings,
// which starts it again behind napio's back. napio still saves over RCON
// first, because /quit does not save.
type GracefulStopper interface {
	// OwnsQuit reports whether Stop performs the clean quit itself.
	OwnsQuit() bool
}

// New builds the backend described by the configuration. It takes the whole
// server because a backend may need to know the endpoint napio guards in order
// to check it against the one the platform actually publishes.
func New(cfg config.Server, log *slog.Logger) (Backend, error) {
	switch cfg.Backend.Type {
	case config.BackendDocker:
		return NewDocker(cfg.Backend.Container, cfg.Backend.Host, log)
	case config.BackendProcess:
		return NewProcess(cfg.Backend.Command, cfg.Backend.WorkingDir, cfg.Backend.Env, log), nil
	case config.BackendPelican, config.BackendPterodactyl:
		return NewWings(cfg.Backend.WingsURL, cfg.Backend.UUID, cfg.Backend.Token,
			cfg.Backend.LogFile, cfg.Backend.TLSSkipVerify, cfg.Address, cfg.Port, log)
	default:
		return nil, fmt.Errorf("unknown backend type %q", cfg.Backend.Type)
	}
}
