package factorio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"git.naknu.li/emily/napio/internal/rcon"
)

// Console commands used for shutdown. Both are RCON-only additions to the
// Factorio console, confirmed by "/help" on a live 2.0.77 server:
//
//	/quit - Terminates the server cleanly.
//	/save <name> - Saves the current game with the specified name.
//
// /quit does NOT save on its own (verified against a running server), so the
// save is always explicit.
const (
	SaveCommand = "/save"
	QuitCommand = "/quit"
)

// ErrNoRCON is returned by a Monitor that has no RCON configured.
var ErrNoRCON = errors.New("rcon is not configured for this server")

// Monitor observes and controls a running Factorio server. Everything it can
// do requires the server to be up; a failing probe means "not ready", which is
// exactly the signal the supervisor wants.
type Monitor interface {
	// Probe reports the connected players. An error means the server is not
	// (yet) ready to accept players.
	Probe(ctx context.Context) (Players, error)
	// Shutdown asks the server to save and quit. It returns once the server
	// has acknowledged, not once the process has exited.
	Shutdown(ctx context.Context) error
	// Available reports whether this monitor can do anything at all.
	Available() bool
}

// RCONMonitor is the real Monitor, backed by the Factorio RCON interface.
//
// A fresh connection is made for every probe. That costs one local TCP
// handshake every poll interval, and in exchange the probe doubles as a
// liveness check with no reconnect state machine to get wrong.
type RCONMonitor struct {
	Address  string
	Password string
	Timeout  time.Duration
	Log      *slog.Logger
}

// NewRCONMonitor builds a monitor. An empty address yields a monitor that
// reports itself unavailable rather than a nil interface.
func NewRCONMonitor(address, password string, timeout time.Duration, log *slog.Logger) *RCONMonitor {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &RCONMonitor{Address: address, Password: password, Timeout: timeout, Log: log}
}

func (m *RCONMonitor) Available() bool { return m.Address != "" }

func (m *RCONMonitor) Probe(ctx context.Context) (Players, error) {
	if !m.Available() {
		return Players{}, ErrNoRCON
	}
	conn, err := rcon.Dial(ctx, m.Address, m.Password, m.Timeout)
	if err != nil {
		return Players{}, err
	}
	defer conn.Close()

	reply, err := conn.Exec(PlayersOnlineCommand)
	if err != nil {
		return Players{}, err
	}
	return ParsePlayersOnline(reply)
}

func (m *RCONMonitor) Shutdown(ctx context.Context) error {
	if !m.Available() {
		return ErrNoRCON
	}
	conn, err := rcon.Dial(ctx, m.Address, m.Password, m.Timeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	if reply, err := conn.Exec(SaveCommand); err != nil {
		return fmt.Errorf("saving before shutdown: %w", err)
	} else if m.Log != nil {
		m.Log.Info("saved game before shutdown", "reply", trim(reply))
	}

	// /quit terminates the server, so a reply is not guaranteed to arrive.
	if err := conn.ExecNoReply(QuitCommand); err != nil {
		return fmt.Errorf("sending /quit: %w", err)
	}
	return nil
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
