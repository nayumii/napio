package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"git.naknu.li/emily/napio/internal/backend"
	"git.naknu.li/emily/napio/internal/config"
	"git.naknu.li/emily/napio/internal/factorio"
	"git.naknu.li/emily/napio/internal/gateway"
	"git.naknu.li/emily/napio/internal/matchmaking"
)

// Publisher keeps a sleeping server visible in the public server browser.
//
// napio publishes a listing under exactly the same condition that it holds the
// UDP port and can actually wake the server — state offline. In every other
// state the listing is withdrawn: while the server runs it publishes its own,
// and while napio is backing off from a failure there is nothing honest to
// advertise.
type Publisher interface {
	Publish()
	Withdraw()
	Status() matchmaking.Status
}

// Listener is the part of the gateway the supervisor drives. It is an
// interface so the state machine can be tested without binding real ports.
type Listener interface {
	Bind() error
	Release()
	Bound() bool
	Wake() <-chan struct{}
}

// Tunables that are deliberately not configurable: they are implementation
// details rather than deployment choices.
const (
	readinessPollInterval = 1 * time.Second
	portFreeTimeout       = 15 * time.Second
	minBackoff            = 30 * time.Second
	maxBackoff            = 5 * time.Minute
	crashWindow           = 10 * time.Minute
	crashesBeforeError    = 3
)

// Supervisor drives one Factorio server through its lifecycle.
//
// Every state transition happens on the Run goroutine, which is what makes the
// socket-ownership invariant hold without lock discipline scattered across the
// package. External callers (the API) post requests through channels and read
// status from a snapshot.
type Supervisor struct {
	cfg       config.Server
	backend   backend.Backend
	monitor   factorio.Monitor
	listener  Listener
	publisher Publisher
	log       *slog.Logger

	// portFree waits until the UDP port can be bound again; injectable so
	// tests do not need real sockets.
	portFree func(addr string, timeout time.Duration) error
	now      func() time.Time
	// readyPoll is how often readiness is probed during startup.
	readyPoll time.Duration

	control chan request

	mu       sync.RWMutex
	snapshot Status

	// Loop-owned state. Only touched from Run.
	state        State
	since        time.Time
	players      *factorio.Players
	idleSince    time.Time
	lastError    string
	rconFailures int
	crashes      []time.Time
	backoffUntil time.Time
	backoff      time.Duration
}

type request struct {
	action string // "start" or "stop"
	reason string
	result chan error
}

// New builds a supervisor for one configured server. publisher may be nil,
// which is the default: no public listing is published.
func New(cfg config.Server, be backend.Backend, mon factorio.Monitor, lis Listener, publisher Publisher, log *slog.Logger) *Supervisor {
	s := &Supervisor{
		cfg:       cfg,
		backend:   be,
		monitor:   mon,
		listener:  lis,
		publisher: publisher,
		log:       log.With("server", cfg.Name),
		portFree:  gateway.WaitUntilFree,
		now:       time.Now,
		readyPoll: readinessPollInterval,
		control:   make(chan request, 8),
		state:     StateOffline,
	}
	s.publish()
	return s
}

// Name is the configured server name.
func (s *Supervisor) Name() string { return s.cfg.Name }

// Status returns the latest snapshot. Safe to call from any goroutine.
func (s *Supervisor) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// RequestStart asks for the server to be started. It returns once the request
// has been accepted, not once the server is online.
func (s *Supervisor) RequestStart(ctx context.Context, reason string) error {
	return s.submit(ctx, "start", reason)
}

// RequestStop asks for a graceful shutdown.
func (s *Supervisor) RequestStop(ctx context.Context, reason string) error {
	return s.submit(ctx, "stop", reason)
}

func (s *Supervisor) submit(ctx context.Context, action, reason string) error {
	req := request{action: action, reason: reason, result: make(chan error, 1)}
	select {
	case s.control <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return errors.New("napio is busy with another transition, try again shortly")
	}
	select {
	case err := <-req.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run drives the state machine until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	s.reconcile(ctx)

	ticker := time.NewTicker(s.cfg.PollInterval.Duration())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("supervisor stopping", "state", s.state)
			if s.publisher != nil {
				s.publisher.Withdraw()
			}
			s.listener.Release()
			return
		case <-s.listener.Wake():
			s.onWake(ctx)
		case req := <-s.control:
			s.onRequest(ctx, req)
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// reconcile adopts whatever is already true when napio starts. napio keeps no
// persistent state: a container that is already running is simply adopted.
func (s *Supervisor) reconcile(ctx context.Context) {
	running, err := s.backend.IsRunning(ctx)
	if err != nil {
		s.log.Error("cannot inspect backend at startup", "error", err, "backend", s.backend.Describe())
		s.fail(fmt.Sprintf("backend unavailable: %v", err))
		return
	}
	if running {
		s.log.Info("adopting already-running server", "backend", s.backend.Describe())
		s.listener.Release()
		s.setState(StateOnline)
		s.idleSince = time.Time{}
		return
	}

	s.setState(StateOffline)
	s.bind()
	if s.cfg.StartOnBoot {
		s.log.Info("start_on_boot is set, starting server")
		s.start(ctx, "start_on_boot")
	}
}

func (s *Supervisor) onWake(_ context.Context) {
	if s.state != StateOffline {
		s.log.Debug("ignoring wake-up traffic", "state", s.state)
		return
	}
	s.log.Info("waking server: a player is trying to connect")
	s.start(context.Background(), "connection attempt")
}

func (s *Supervisor) onRequest(ctx context.Context, req request) {
	switch req.action {
	case "start":
		switch s.state {
		case StateOnline:
			req.result <- nil
		case StateStarting, StateStopping:
			req.result <- fmt.Errorf("server is %s", s.state)
		default:
			s.backoffUntil = time.Time{} // an explicit request clears the backoff
			s.backoff = 0
			req.result <- s.start(ctx, req.reason)
		}
	case "stop":
		switch s.state {
		case StateOffline:
			req.result <- nil
		case StateStarting, StateStopping:
			req.result <- fmt.Errorf("server is %s", s.state)
		default:
			req.result <- s.stop(ctx, req.reason)
		}
	default:
		req.result <- fmt.Errorf("unknown action %q", req.action)
	}
}

func (s *Supervisor) tick(ctx context.Context) {
	switch s.state {
	case StateOffline:
		s.tickOffline(ctx)
	case StateOnline:
		s.tickOnline(ctx)
	case StateError:
		s.tickError(ctx)
	}
	// The listing is published asynchronously, so the snapshot has to be
	// refreshed on the tick as well as on state changes.
	s.publish()
}

func (s *Supervisor) tickOffline(ctx context.Context) {
	// Someone may have started the server outside napio; adopt it rather than
	// fight it.
	if running, err := s.backend.IsRunning(ctx); err == nil && running {
		s.log.Info("server was started outside napio, adopting it")
		s.listener.Release()
		s.setState(StateOnline)
		s.idleSince = time.Time{}
		return
	}
	// A previous bind may have failed (for example the port was still in
	// TIME_WAIT or held by a stopping container); keep trying.
	if !s.listener.Bound() {
		s.bind()
	}
}

func (s *Supervisor) tickError(ctx context.Context) {
	if !s.listener.Bound() {
		if running, err := s.backend.IsRunning(ctx); err == nil && !running {
			s.bind()
		}
	}
	if s.backoffUntil.IsZero() || s.now().After(s.backoffUntil) {
		s.log.Info("backoff elapsed, returning to offline")
		s.lastError = ""
		s.setState(StateOffline)
		s.bind()
	}
}

func (s *Supervisor) tickOnline(ctx context.Context) {
	running, err := s.backend.IsRunning(ctx)
	if err != nil {
		s.log.Warn("cannot inspect backend", "error", err)
		return
	}
	if !running {
		s.onCrash()
		return
	}

	if !s.monitor.Available() {
		// Without RCON napio cannot see players, so it must not decide that
		// the server is idle. This is documented, and warned about at startup.
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	players, err := s.monitor.Probe(probeCtx)
	cancel()
	if err != nil {
		s.rconFailures++
		s.log.Warn("player probe failed; inactivity countdown paused",
			"error", err, "consecutive_failures", s.rconFailures)
		if s.rconFailures == s.rconTolerance() {
			s.lastError = fmt.Sprintf("rcon unreachable: %v", err)
			s.publish()
		}
		// The countdown is deliberately not advanced: an unknown player count
		// must never be treated as zero players.
		return
	}
	if s.rconFailures > 0 {
		s.log.Info("player probe recovered", "after_failures", s.rconFailures)
		s.rconFailures = 0
		s.lastError = ""
	}

	s.players = &players
	if players.Count > 0 {
		if !s.idleSince.IsZero() {
			s.log.Info("inactivity countdown cancelled", "players", players.Count)
		}
		s.idleSince = time.Time{}
		s.publish()
		return
	}

	if s.idleSince.IsZero() {
		s.idleSince = s.now()
		s.log.Info("server is empty, inactivity countdown started",
			"timeout", s.cfg.InactivityTimeout.Duration())
	}
	s.publish()

	if s.now().Sub(s.idleSince) >= s.cfg.InactivityTimeout.Duration() {
		s.log.Info("inactivity timeout reached, shutting down",
			"idle_for", s.now().Sub(s.idleSince).Truncate(time.Second))
		if err := s.stop(ctx, "inactivity timeout"); err != nil {
			s.log.Error("automatic shutdown failed", "error", err)
		}
	}
}

// start takes the server from offline to online. It runs inline on the Run
// goroutine: there is nothing else to do for this server while it starts, and
// serialising it removes every start/stop race.
func (s *Supervisor) start(ctx context.Context, reason string) error {
	if !s.backoffUntil.IsZero() && s.now().Before(s.backoffUntil) {
		wait := s.backoffUntil.Sub(s.now()).Truncate(time.Second)
		s.log.Warn("not starting: backing off after a previous failure", "retry_in", wait)
		return fmt.Errorf("backing off after a previous failure, retrying in %s", wait)
	}

	s.setState(StateStarting)
	s.log.Info("starting server", "reason", reason, "backend", s.backend.Describe())

	// Release the port BEFORE starting: Docker refuses to start a container
	// whose published port is already held (verified against a live daemon).
	s.listener.Release()

	startCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout.Duration())
	defer cancel()

	if err := s.backend.Start(startCtx); err != nil {
		s.log.Error("backend failed to start the server", "error", err)
		s.fail(err.Error())
		return err
	}

	if err := s.waitReady(startCtx); err != nil {
		s.log.Error("server did not become ready", "error", err)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), s.cfg.StopTimeout.Duration())
		if stopErr := s.backend.Stop(stopCtx, s.cfg.StopTimeout.Duration()); stopErr != nil {
			s.log.Error("could not stop the failed server", "error", stopErr)
		}
		stopCancel()
		s.fail(err.Error())
		return err
	}

	s.backoff = 0
	s.backoffUntil = time.Time{}
	s.lastError = ""
	s.rconFailures = 0
	s.idleSince = time.Time{}
	s.setState(StateOnline)
	s.log.Info("server is online")
	return nil
}

// waitReady blocks until Factorio is ready to accept players.
//
// The readiness signal is the RCON interface answering, because the Factorio
// server only starts RCON after the save is loaded and the game has reached
// the InGame state (verified in the server log). Without RCON, napio can only
// tell that the process exists, which is documented as a degraded mode.
func (s *Supervisor) waitReady(ctx context.Context) error {
	if !s.monitor.Available() {
		running, err := s.backend.IsRunning(ctx)
		if err != nil {
			return err
		}
		if !running {
			return errors.New("process exited immediately after start")
		}
		s.log.Warn("no rcon configured: treating 'process running' as ready, " +
			"which cannot detect a save that fails to load")
		return nil
	}

	ticker := time.NewTicker(s.readyPoll)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("server did not become ready within %s (last probe error: %v)",
					s.cfg.StartTimeout, lastErr)
			}
			return fmt.Errorf("server did not become ready within %s", s.cfg.StartTimeout)
		case <-ticker.C:
		}

		running, err := s.backend.IsRunning(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		if !running {
			return errors.New("server exited during startup (check the Factorio log: a corrupt save or a mod mismatch will do this)")
		}

		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err = s.monitor.Probe(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
	}
}

// stop takes the server from online to offline, gracefully.
func (s *Supervisor) stop(ctx context.Context, reason string) error {
	s.setState(StateStopping)
	s.log.Info("stopping server", "reason", reason)

	timeout := s.cfg.StopTimeout.Duration()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	// Preferred path: ask Factorio to save and quit itself. /quit does not
	// save on its own, so the save is explicit and comes first.
	if s.monitor.Available() {
		if err := s.monitor.Shutdown(stopCtx); err != nil {
			s.log.Warn("graceful rcon shutdown failed, falling back to the backend", "error", err)
		} else if err := s.waitExit(stopCtx, timeout/2); err == nil {
			return s.finishStop(stopCtx)
		} else {
			s.log.Warn("server did not exit after /quit, falling back to the backend", "error", err)
		}
	}

	// Fallback: SIGTERM through the backend. The Factorio headless server
	// saves the map when signalled, so this is still a clean shutdown.
	if err := s.backend.Stop(stopCtx, timeout); err != nil {
		s.log.Error("backend stop failed", "error", err)
		s.fail(fmt.Sprintf("stop failed: %v", err))
		return err
	}
	if err := s.waitExit(stopCtx, timeout); err != nil {
		s.fail(fmt.Sprintf("server did not stop: %v", err))
		return err
	}
	return s.finishStop(stopCtx)
}

// finishStop rebinds the listener once the port is genuinely free. Binding
// early would either fail or, worse, race the server for the port.
func (s *Supervisor) finishStop(ctx context.Context) error {
	if err := s.portFree(s.cfg.UDPAddr(), portFreeTimeout); err != nil {
		s.log.Error("udp port did not become free after shutdown", "error", err)
		s.fail(err.Error())
		return err
	}
	s.players = nil
	s.idleSince = time.Time{}
	s.rconFailures = 0
	s.setState(StateOffline)
	s.bind()
	s.log.Info("server is offline and napio is listening for the next player")
	return nil
}

func (s *Supervisor) waitExit(ctx context.Context, timeout time.Duration) error {
	deadline := s.now().Add(timeout)
	for {
		running, err := s.backend.IsRunning(ctx)
		if err == nil && !running {
			return nil
		}
		if s.now().After(deadline) {
			if err != nil {
				return fmt.Errorf("still running after %s: %w", timeout, err)
			}
			return fmt.Errorf("still running after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// onCrash handles the server disappearing while it was supposed to be online.
// A single crash returns to offline so the next player simply wakes it again;
// repeated crashes are an error worth backing off from and surfacing.
func (s *Supervisor) onCrash() {
	now := s.now()
	s.crashes = append(s.crashes, now)
	kept := s.crashes[:0]
	for _, t := range s.crashes {
		if now.Sub(t) <= crashWindow {
			kept = append(kept, t)
		}
	}
	s.crashes = kept

	s.players = nil
	s.idleSince = time.Time{}

	if len(s.crashes) >= crashesBeforeError {
		s.log.Error("server crashed repeatedly, backing off",
			"crashes", len(s.crashes), "window", crashWindow)
		s.fail(fmt.Sprintf("server crashed %d times within %s", len(s.crashes), crashWindow))
		return
	}
	s.log.Warn("server stopped unexpectedly, returning to offline", "crashes_in_window", len(s.crashes))
	s.setState(StateOffline)
	s.bind()
}

// fail moves to the error state with an exponential backoff, and takes the
// port back so a player can still trigger a retry once the backoff elapses.
func (s *Supervisor) fail(message string) {
	s.lastError = message
	if s.backoff == 0 {
		s.backoff = minBackoff
	} else {
		s.backoff *= 2
		if s.backoff > maxBackoff {
			s.backoff = maxBackoff
		}
	}
	s.backoffUntil = s.now().Add(s.backoff)
	s.setState(StateError)
	s.bind()
}

// bind takes the UDP port, but only in a state that permits it.
func (s *Supervisor) bind() {
	if !s.state.HoldsPort() {
		return
	}
	if err := s.listener.Bind(); err != nil {
		s.log.Error("cannot bind the wake-up port; players will not be able to wake this server",
			"address", s.cfg.UDPAddr(), "error", err)
		s.lastError = err.Error()
	}
	s.publish()
}

func (s *Supervisor) setState(state State) {
	if s.state != state {
		s.log.Info("state changed", "from", s.state, "to", state)
	}
	s.state = state
	s.since = s.now()
	s.syncListing()
	s.publish()
}

// syncListing keeps the public listing in step with the state: advertised only
// while the server is asleep and napio can actually wake it.
func (s *Supervisor) syncListing() {
	if s.publisher == nil {
		return
	}
	if s.state == StateOffline {
		s.publisher.Publish()
		return
	}
	s.publisher.Withdraw()
}

func (s *Supervisor) rconTolerance() int {
	if s.cfg.RCON.FailureTolerance > 0 {
		return s.cfg.RCON.FailureTolerance
	}
	return config.DefaultRCONFailureTolerance
}

func (s *Supervisor) publish() {
	status := Status{
		Name:                     s.cfg.Name,
		State:                    s.state,
		Address:                  s.cfg.UDPAddr(),
		Backend:                  s.backend.Describe(),
		Since:                    s.since,
		Players:                  s.players,
		InactivityTimeoutSeconds: int64(s.cfg.InactivityTimeout.Duration().Seconds()),
		AutoShutdown:             s.monitor.Available(),
		ListenerBound:            s.listener.Bound(),
		LastError:                s.lastError,
	}
	if s.state == StateOnline && !s.idleSince.IsZero() {
		idle := int64(s.now().Sub(s.idleSince).Seconds())
		remaining := int64(s.cfg.InactivityTimeout.Duration().Seconds()) - idle
		if remaining < 0 {
			remaining = 0
		}
		status.IdleSeconds = &idle
		status.ShutdownInSeconds = &remaining
	}
	if s.publisher != nil {
		listing := s.publisher.Status()
		status.Listing = &listing
	}
	if s.state == StateError && !s.backoffUntil.IsZero() {
		retry := int64(s.backoffUntil.Sub(s.now()).Seconds())
		if retry < 0 {
			retry = 0
		}
		status.RetryInSeconds = &retry
	}

	s.mu.Lock()
	s.snapshot = status
	s.mu.Unlock()
}
