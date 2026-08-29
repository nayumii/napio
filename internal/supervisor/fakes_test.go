package supervisor

import (
	"context"
	"errors"
	"sync"
	"time"

	"git.naknu.li/emily/napio/internal/config"
	"git.naknu.li/emily/napio/internal/factorio"
	"git.naknu.li/emily/napio/internal/matchmaking"
)

// fakeBackend stands in for Docker. It records what happened and can be told
// to fail in the ways a real deployment fails.
type fakeBackend struct {
	mu sync.Mutex

	running bool
	starts  int
	stops   int

	startErr error
	stopErr  error
	// exitAfterStart simulates a server that dies immediately (bad save, mod
	// mismatch, out of memory).
	exitAfterStart bool
	// refusePortHeld reproduces Docker's real behaviour: starting fails while
	// something else still holds the published UDP port.
	listener *fakeListener
}

func (f *fakeBackend) Describe() string { return "fake backend" }

func (f *fakeBackend) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.listener != nil && f.listener.Bound() {
		return errors.New("port is still held by napio (docker would refuse this)")
	}
	if f.startErr != nil {
		return f.startErr
	}
	f.running = !f.exitAfterStart
	return nil
}

func (f *fakeBackend) Stop(context.Context, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.running = false
	return nil
}

func (f *fakeBackend) IsRunning(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}

func (f *fakeBackend) crash() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
}

func (f *fakeBackend) startExternally() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
}

func (f *fakeBackend) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// fakeMonitor stands in for RCON.
type fakeMonitor struct {
	mu sync.Mutex

	available bool
	players   int
	probeErr  error

	shutdownErr    error
	shutdowns      int
	shutdownStops  *fakeBackend // when set, a successful shutdown stops it
	readyAfterCall int          // probes fail until this many calls have been made
	probes         int
}

func (m *fakeMonitor) Available() bool { return m.available }

func (m *fakeMonitor) Probe(context.Context) (factorio.Players, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probes++
	if m.probes <= m.readyAfterCall {
		return factorio.Players{}, errors.New("connection refused")
	}
	if m.probeErr != nil {
		return factorio.Players{}, m.probeErr
	}
	return factorio.Players{Count: m.players}, nil
}

func (m *fakeMonitor) Shutdown(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdowns++
	if m.shutdownErr != nil {
		return m.shutdownErr
	}
	if m.shutdownStops != nil {
		m.shutdownStops.mu.Lock()
		m.shutdownStops.running = false
		m.shutdownStops.mu.Unlock()
	}
	return nil
}

func (m *fakeMonitor) setPlayers(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.players = n
}

func (m *fakeMonitor) setProbeErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probeErr = err
}

func (m *fakeMonitor) shutdownCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shutdowns
}

// fakeListener records bind/release without touching a real socket.
type fakeListener struct {
	mu       sync.Mutex
	bound    bool
	binds    int
	releases int
	wake     chan struct{}
	bindErr  error
}

func newFakeListener() *fakeListener {
	return &fakeListener{wake: make(chan struct{}, 1)}
}

func (l *fakeListener) Bind() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bindErr != nil {
		return l.bindErr
	}
	l.binds++
	l.bound = true
	return nil
}

func (l *fakeListener) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bound {
		l.releases++
	}
	l.bound = false
}

func (l *fakeListener) Bound() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bound
}

func (l *fakeListener) Wake() <-chan struct{} { return l.wake }

// trigger simulates a player's connection attempt passing the wake filter.
func (l *fakeListener) trigger() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// fakePublisher records how the listing follows the state machine.
type fakePublisher struct {
	mu        sync.Mutex
	published int
	withdrawn int
	listed    bool
}

func (p *fakePublisher) Publish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.listed {
		p.published++
	}
	p.listed = true
}

func (p *fakePublisher) Withdraw() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listed {
		p.withdrawn++
	}
	p.listed = false
}

func (p *fakePublisher) Status() matchmaking.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return matchmaking.Status{Enabled: true, Listed: p.listed}
}

func (p *fakePublisher) isListed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listed
}

// testConfig builds a server configuration with millisecond timings so tests
// exercise real timers without being slow.
func testConfig(name string) config.Server {
	return config.Server{
		Name:              name,
		Address:           "127.0.0.1",
		Port:              34197,
		InactivityTimeout: config.Duration(120 * time.Millisecond),
		StartTimeout:      config.Duration(2 * time.Second),
		StopTimeout:       config.Duration(1 * time.Second),
		PollInterval:      config.Duration(10 * time.Millisecond),
		RCON:              config.RCON{Address: "127.0.0.1:27015", Password: "x", FailureTolerance: 3},
		Backend:           config.Backend{Type: config.BackendDocker, Container: name},
	}
}
