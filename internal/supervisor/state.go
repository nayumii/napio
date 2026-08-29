// Package supervisor contains napio's core: one state machine per Factorio
// server, owning the transition between "asleep, napio holds the port" and
// "awake, Factorio holds the port".
package supervisor

import (
	"time"

	"git.naknu.li/emily/napio/internal/factorio"
	"git.naknu.li/emily/napio/internal/matchmaking"
)

// State is the lifecycle state of one managed server.
type State string

const (
	// StateOffline means the server is not running and napio holds the UDP
	// port, waiting for a player.
	StateOffline State = "offline"
	// StateStarting means the server is being started and napio has released
	// the port.
	StateStarting State = "starting"
	// StateOnline means the server is running and accepting players. napio is
	// not in the network path at all.
	StateOnline State = "online"
	// StateStopping means the server is being shut down gracefully.
	StateStopping State = "stopping"
	// StateError means something failed and napio is backing off before it
	// tries again. napio holds the port so players still trigger a retry.
	StateError State = "error"
)

// HoldsPort reports whether napio should own the UDP listener in this state.
// This is the socket-ownership invariant: napio holds the port if and only if
// the server does not.
func (s State) HoldsPort() bool { return s == StateOffline || s == StateError }

// Status is a point-in-time snapshot for the API and for logging.
type Status struct {
	Name    string `json:"name"`
	State   State  `json:"state"`
	Address string `json:"address"`
	Backend string `json:"backend"`

	Since   time.Time         `json:"since"`
	Players *factorio.Players `json:"players,omitempty"`

	// IdleSeconds is how long the server has had zero players, or nil when it
	// is not online or the player count is unknown.
	IdleSeconds *int64 `json:"idle_seconds,omitempty"`
	// ShutdownInSeconds is the remaining inactivity countdown, if running.
	ShutdownInSeconds *int64 `json:"shutdown_in_seconds,omitempty"`

	InactivityTimeoutSeconds int64  `json:"inactivity_timeout_seconds"`
	AutoShutdown             bool   `json:"auto_shutdown"`
	ListenerBound            bool   `json:"listener_bound"`
	LastError                string `json:"last_error,omitempty"`
	RetryInSeconds           *int64 `json:"retry_in_seconds,omitempty"`

	// Listing reports the public server-browser listing, when napio is
	// configured to publish one. It never contains credentials.
	Listing *matchmaking.Status `json:"listing,omitempty"`
}
