package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"git.naknu.li/emily/napio/internal/config"
)

func newTestSupervisor(t *testing.T, cfg func(*testHarness)) *testHarness {
	t.Helper()
	h := &testHarness{
		backend:  &fakeBackend{},
		monitor:  &fakeMonitor{available: true},
		listener: newFakeListener(),
		cfg:      testConfig("test"),
	}
	if cfg != nil {
		cfg(h)
	}
	// The fake backend refuses to start while the listener is bound, exactly
	// as Docker does: this makes the port-handover ordering a tested property
	// of every start in this file, not a special case.
	h.backend.listener = h.listener

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.sup = New(h.cfg, h.backend, h.monitor, h.listener, log)
	h.sup.portFree = func(string, time.Duration) error { return nil }
	h.sup.readyPoll = 5 * time.Millisecond
	return h
}

type testHarness struct {
	cfg      config.Server
	backend  *fakeBackend
	monitor  *fakeMonitor
	listener *fakeListener
	sup      *Supervisor
	cancel   context.CancelFunc
}

func (h *testHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	done := make(chan struct{})
	go func() { defer close(done); h.sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not stop")
		}
	})
}

// waitFor polls the published status until cond holds. Polling the snapshot
// keeps the tests free of any privileged access to the loop's internals.
func (h *testHarness) waitFor(t *testing.T, desc string, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last Status
	for time.Now().Before(deadline) {
		last = h.sup.Status()
		if cond(last) {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; state=%q bound=%v error=%q",
		desc, last.State, last.ListenerBound, last.LastError)
	return last
}

func (h *testHarness) waitState(t *testing.T, want State) Status {
	t.Helper()
	return h.waitFor(t, "state "+string(want), func(s Status) bool { return s.State == want })
}

// waitSleeping waits for the fully settled asleep condition: offline AND
// holding the port, which is the invariant a player depends on.
func (h *testHarness) waitSleeping(t *testing.T) Status {
	t.Helper()
	return h.waitFor(t, "offline and holding the port", func(s Status) bool {
		return s.State == StateOffline && s.ListenerBound
	})
}

func TestOfflineAtStartupAndHoldsThePort(t *testing.T) {
	h := newTestSupervisor(t, nil)
	h.run(t)

	status := h.waitSleeping(t)
	if !status.ListenerBound {
		t.Fatal("napio must hold the UDP port while the server is offline")
	}
	if starts, _ := h.backend.counts(); starts != 0 {
		t.Fatalf("nothing should have been started, got %d starts", starts)
	}
}

func TestWakeTrafficStartsTheServer(t *testing.T) {
	h := newTestSupervisor(t, nil)
	h.run(t)
	h.waitSleeping(t)

	h.listener.trigger()

	status := h.waitState(t, StateOnline)
	if status.ListenerBound {
		t.Fatal("napio must not hold the UDP port while the server is online")
	}
	if starts, _ := h.backend.counts(); starts != 1 {
		t.Fatalf("expected exactly one start, got %d", starts)
	}
	if h.listener.releases != 1 {
		t.Fatalf("expected the port to be released once before starting, got %d", h.listener.releases)
	}
}

func TestWakeDuringStartupDoesNotStartTwice(t *testing.T) {
	// A player who retries while the server is still loading must not cause a
	// second start.
	h := newTestSupervisor(t, func(h *testHarness) {
		h.monitor.readyAfterCall = 3 // stay in STARTING for a few probes
	})
	h.run(t)
	h.waitSleeping(t)

	h.listener.trigger()
	for i := 0; i < 5; i++ {
		h.listener.trigger()
		time.Sleep(2 * time.Millisecond)
	}

	h.waitState(t, StateOnline)
	time.Sleep(50 * time.Millisecond)
	if starts, _ := h.backend.counts(); starts != 1 {
		t.Fatalf("expected exactly one start despite repeated connection attempts, got %d", starts)
	}
}

func TestStartFailureEntersErrorAndTakesThePortBack(t *testing.T) {
	h := newTestSupervisor(t, func(h *testHarness) {
		h.backend.startErr = errors.New("no such container")
	})
	h.run(t)
	h.waitSleeping(t)

	h.listener.trigger()

	status := h.waitFor(t, "error state holding the port", func(s Status) bool {
		return s.State == StateError && s.ListenerBound
	})
	if !status.ListenerBound {
		t.Fatal("napio must take the port back after a failed start so players can retry")
	}
	if status.LastError == "" {
		t.Fatal("the failure should be reported in the status")
	}
	if status.RetryInSeconds == nil {
		t.Fatal("a backoff should be reported")
	}
}

func TestServerThatNeverBecomesReadyIsStoppedAgain(t *testing.T) {
	h := newTestSupervisor(t, func(h *testHarness) {
		h.cfg.StartTimeout = config.Duration(120 * time.Millisecond)
		h.monitor.probeErr = errors.New("connection refused")
	})
	h.run(t)
	h.waitSleeping(t)

	h.listener.trigger()

	status := h.waitFor(t, "error state holding the port", func(s Status) bool {
		return s.State == StateError && s.ListenerBound
	})
	if _, stops := h.backend.counts(); stops == 0 {
		t.Fatal("a server that never becomes ready must be stopped again")
	}
	if !status.ListenerBound {
		t.Fatal("napio must hold the port again after a failed startup")
	}
}

func TestServerExitingDuringStartupIsReportedAsFailure(t *testing.T) {
	h := newTestSupervisor(t, func(h *testHarness) {
		h.backend.exitAfterStart = true
		h.monitor.probeErr = errors.New("connection refused")
		h.cfg.StartTimeout = config.Duration(2 * time.Second)
	})
	h.run(t)
	h.waitSleeping(t)

	h.listener.trigger()

	status := h.waitState(t, StateError)
	if status.LastError == "" {
		t.Fatal("expected an error message describing the failed startup")
	}
}

func TestShutsDownAfterInactivityTimeout(t *testing.T) {
	h := newTestSupervisor(t, nil)
	h.monitor.shutdownStops = h.backend
	h.run(t)
	h.waitSleeping(t)

	h.listener.trigger()
	h.waitState(t, StateOnline)

	status := h.waitSleeping(t)
	if !status.ListenerBound {
		t.Fatal("napio must take the port back once the server is asleep")
	}
	if h.monitor.shutdownCount() != 1 {
		t.Fatalf("expected a graceful rcon shutdown, got %d", h.monitor.shutdownCount())
	}
}

func TestPlayerJoiningCancelsTheCountdown(t *testing.T) {
	// A connected player must keep the server up indefinitely, and the
	// countdown must start from zero once they leave.
	h := newTestSupervisor(t, nil)
	h.monitor.shutdownStops = h.backend
	h.monitor.setPlayers(1)
	h.run(t)
	h.waitSleeping(t)
	h.listener.trigger()
	h.waitState(t, StateOnline)

	// Several inactivity timeouts pass with somebody online.
	time.Sleep(5 * h.cfg.InactivityTimeout.Duration())
	if got := h.sup.Status().State; got != StateOnline {
		t.Fatalf("server was shut down with a player connected: state %q", got)
	}
	if h.monitor.shutdownCount() != 0 {
		t.Fatal("no shutdown should have been attempted while a player was online")
	}

	h.monitor.setPlayers(0)
	h.waitSleeping(t)
}

func TestUnknownPlayerCountPausesTheCountdown(t *testing.T) {
	// An unreachable RCON must never be read as "zero players": that would
	// shut down a server full of people.
	h := newTestSupervisor(t, func(h *testHarness) {
		// Long enough that the probe below is guaranteed to be in place well
		// before the countdown could ever expire.
		h.cfg.InactivityTimeout = config.Duration(time.Second)
	})
	h.run(t)
	h.waitSleeping(t)
	h.listener.trigger()
	h.waitState(t, StateOnline)

	h.monitor.setProbeErr(errors.New("connection reset"))

	time.Sleep(3 * time.Second)
	if got := h.sup.Status().State; got != StateOnline {
		t.Fatalf("server was shut down while the player count was unknown: state %q", got)
	}
	if h.monitor.shutdownCount() != 0 {
		t.Fatal("no shutdown should be attempted when the player count is unknown")
	}
}

func TestCrashReturnsToOfflineAndRebinds(t *testing.T) {
	h := newTestSupervisor(t, nil)
	h.run(t)
	h.waitSleeping(t)
	h.listener.trigger()
	h.waitState(t, StateOnline)

	h.backend.crash()

	status := h.waitSleeping(t)
	if !status.ListenerBound {
		t.Fatal("napio must take the port back after a crash so the next player can wake the server")
	}
}

func TestRepeatedCrashesEnterError(t *testing.T) {
	h := newTestSupervisor(t, nil)
	h.run(t)

	for i := 0; i < crashesBeforeError; i++ {
		h.waitSleeping(t)
		h.listener.trigger()
		h.waitState(t, StateOnline)
		h.backend.crash()
	}

	status := h.waitState(t, StateError)
	if status.LastError == "" {
		t.Fatal("repeated crashes should be reported")
	}
}

func TestAdoptsAServerThatIsAlreadyRunning(t *testing.T) {
	// napio keeps no state across restarts: it adopts reality.
	h := newTestSupervisor(t, nil)
	h.backend.startExternally()
	h.run(t)

	status := h.waitState(t, StateOnline)
	if status.ListenerBound {
		t.Fatal("napio must not hold the port of a server that is already running")
	}
	if starts, _ := h.backend.counts(); starts != 0 {
		t.Fatal("an already-running server must not be started again")
	}
}

func TestAdoptsAServerStartedOutsideNapio(t *testing.T) {
	h := newTestSupervisor(t, nil)
	h.run(t)
	h.waitSleeping(t)

	h.listener.Release() // as if the operator started the container by hand
	h.backend.startExternally()

	status := h.waitState(t, StateOnline)
	if status.ListenerBound {
		t.Fatal("napio must release the port for an externally started server")
	}
}

func TestManualStartAndStop(t *testing.T) {
	h := newTestSupervisor(t, func(h *testHarness) {
		h.cfg.InactivityTimeout = config.Duration(10 * time.Second) // no automatic stop
	})
	h.monitor.shutdownStops = h.backend
	h.run(t)
	h.waitSleeping(t)

	if err := h.sup.RequestStart(context.Background(), "test"); err != nil {
		t.Fatalf("manual start: %v", err)
	}
	h.waitState(t, StateOnline)

	if err := h.sup.RequestStop(context.Background(), "test"); err != nil {
		t.Fatalf("manual stop: %v", err)
	}
	h.waitState(t, StateOffline)
}

func TestGracefulShutdownFallsBackToTheBackend(t *testing.T) {
	// If /save + /quit fails, the backend's SIGTERM is the safety net: the
	// Factorio headless server saves when signalled.
	h := newTestSupervisor(t, nil)
	h.monitor.shutdownErr = errors.New("rcon closed")
	h.run(t)
	h.waitSleeping(t)
	h.listener.trigger()
	h.waitState(t, StateOnline)

	h.waitSleeping(t)
	if _, stops := h.backend.counts(); stops == 0 {
		t.Fatal("the backend stop must be used when the rcon shutdown fails")
	}
}

func TestWithoutRCONThereIsNoAutomaticShutdown(t *testing.T) {
	h := newTestSupervisor(t, func(h *testHarness) {
		h.monitor.available = false
	})
	h.run(t)
	h.waitSleeping(t)
	h.listener.trigger()
	status := h.waitState(t, StateOnline)

	if status.AutoShutdown {
		t.Fatal("automatic shutdown must be reported as unavailable without rcon")
	}
	time.Sleep(300 * time.Millisecond)
	if got := h.sup.Status().State; got != StateOnline {
		t.Fatalf("a server without rcon must not be stopped on a guess: state %q", got)
	}
}

func TestMultipleServersAreIndependent(t *testing.T) {
	good := newTestSupervisor(t, func(h *testHarness) {
		h.cfg.Name = "good"
		h.cfg.InactivityTimeout = config.Duration(10 * time.Second)
	})
	bad := newTestSupervisor(t, func(h *testHarness) {
		h.cfg.Name = "bad"
		h.backend.startErr = errors.New("no such container")
	})

	good.run(t)
	bad.run(t)

	good.waitSleeping(t)
	bad.waitSleeping(t)

	bad.listener.trigger()
	good.listener.trigger()

	bad.waitState(t, StateError)
	good.waitState(t, StateOnline)

	if got := bad.sup.Status().Name; got == good.sup.Status().Name {
		t.Fatal("statuses must be reported per server")
	}
}
