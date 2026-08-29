// Package integration exercises napio against a real Factorio server.
//
// These tests are skipped unless NAPIO_INTEGRATION_CONTAINER names a Docker
// container running a Factorio headless server with RCON enabled, so the
// normal test suite (and CI) stays hermetic:
//
//	NAPIO_INTEGRATION_CONTAINER=napio-test \
//	NAPIO_INTEGRATION_RCON_PASSWORD=... \
//	NAPIO_INTEGRATION_RCON_ADDRESS=127.0.0.1:27015 \
//	NAPIO_INTEGRATION_PORT=34197 \
//	go test ./internal/integration/ -v -timeout 10m
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"git.naknu.li/emily/napio/internal/backend"
	"git.naknu.li/emily/napio/internal/config"
	"git.naknu.li/emily/napio/internal/factorio"
	"git.naknu.li/emily/napio/internal/gateway"
	"git.naknu.li/emily/napio/internal/supervisor"
)

type env struct {
	container    string
	rconAddress  string
	rconPassword string
	port         int
}

func requireEnv(t *testing.T) env {
	t.Helper()
	container := os.Getenv("NAPIO_INTEGRATION_CONTAINER")
	if container == "" {
		t.Skip("set NAPIO_INTEGRATION_CONTAINER to run the integration tests")
	}
	port, err := strconv.Atoi(orDefault(os.Getenv("NAPIO_INTEGRATION_PORT"), "34197"))
	if err != nil {
		t.Fatalf("NAPIO_INTEGRATION_PORT: %v", err)
	}
	return env{
		container:    container,
		rconAddress:  orDefault(os.Getenv("NAPIO_INTEGRATION_RCON_ADDRESS"), "127.0.0.1:27015"),
		rconPassword: os.Getenv("NAPIO_INTEGRATION_RCON_PASSWORD"),
		port:         port,
	}
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// TestWakeAndSleepAgainstARealServer is the end-to-end proof of the port
// handover: napio holds the UDP port, a connection attempt starts the real
// Factorio container, napio gets out of the way, and once the server has been
// empty long enough it is saved, stopped and the port taken back.
func TestWakeAndSleepAgainstARealServer(t *testing.T) {
	e := requireEnv(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := config.Server{
		Name:              "integration",
		Address:           "0.0.0.0",
		Port:              e.port,
		InactivityTimeout: config.Duration(15 * time.Second),
		StartTimeout:      config.Duration(3 * time.Minute),
		StopTimeout:       config.Duration(1 * time.Minute),
		PollInterval:      config.Duration(2 * time.Second),
		RCON: config.RCON{
			Address:          e.rconAddress,
			Password:         e.rconPassword,
			FailureTolerance: 3,
		},
		Backend: config.Backend{Type: config.BackendDocker, Container: e.container},
	}

	be, err := backend.New(cfg.Backend, log)
	if err != nil {
		t.Fatalf("building backend: %v", err)
	}

	// Start from a known state: the server stopped.
	ctx := context.Background()
	if err := be.Stop(ctx, 30*time.Second); err != nil {
		t.Fatalf("stopping the container before the test: %v", err)
	}
	if err := gateway.WaitUntilFree(cfg.UDPAddr(), 30*time.Second); err != nil {
		t.Fatalf("waiting for the port: %v", err)
	}

	mon := factorio.NewRCONMonitor(cfg.RCON.Address, cfg.RCON.Password, 5*time.Second, log)
	filter := gateway.NewWakeFilter(4, 2, 5*time.Second, 5*time.Second, nil)
	listener := gateway.NewListener(cfg.UDPAddr(), filter, log)
	sup := supervisor.New(cfg, be, mon, listener, nil, log)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(runCtx) }()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, 30*time.Second, "napio to hold the port while the server sleeps", func() bool {
		s := sup.Status()
		return s.State == supervisor.StateOffline && s.ListenerBound
	})

	// A player tries to connect: the Factorio client retries, which is what
	// the wake filter looks for.
	knock(t, fmt.Sprintf("127.0.0.1:%d", e.port), 3)

	waitFor(t, 4*time.Minute, "the real Factorio server to come online", func() bool {
		return sup.Status().State == supervisor.StateOnline
	})

	status := sup.Status()
	if status.ListenerBound {
		t.Fatal("napio must release the port once Factorio owns it")
	}
	if running, err := be.IsRunning(ctx); err != nil || !running {
		t.Fatalf("the container should be running: running=%v err=%v", running, err)
	}
	// Readiness means Factorio really is answering, not just that the process exists.
	players, err := mon.Probe(ctx)
	if err != nil {
		t.Fatalf("the server should be answering rcon once online: %v", err)
	}
	if players.Count != 0 {
		t.Fatalf("expected an empty server, got %d players", players.Count)
	}

	// Nobody joins, so the inactivity timeout must put it back to sleep.
	waitFor(t, 3*time.Minute, "the server to be shut down and the port taken back", func() bool {
		s := sup.Status()
		return s.State == supervisor.StateOffline && s.ListenerBound
	})

	if running, err := be.IsRunning(ctx); err != nil || running {
		t.Fatalf("the container should be stopped: running=%v err=%v", running, err)
	}
}

// knock sends datagrams the way a Factorio client retries its connection
// request. napio never looks at the contents.
func knock(t *testing.T, address string, count int) {
	t.Helper()
	conn, err := net.Dial("udp", address)
	if err != nil {
		t.Fatalf("dialling %s: %v", address, err)
	}
	defer conn.Close()
	for i := 0; i < count; i++ {
		if _, err := conn.Write([]byte("napio integration test connection attempt")); err != nil {
			t.Fatalf("sending a datagram: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			t.Logf("reached: %s", what)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}
