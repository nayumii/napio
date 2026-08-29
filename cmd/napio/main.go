// Command napio is a wake-on-connect gateway for Factorio dedicated servers.
//
// It holds each server's UDP port while the server is asleep, starts the
// server when a player tries to connect, gets completely out of the network
// path while the server runs, and shuts the server down again once it has been
// empty for long enough.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"git.naknu.li/emily/napio/internal/api"
	"git.naknu.li/emily/napio/internal/backend"
	"git.naknu.li/emily/napio/internal/config"
	"git.naknu.li/emily/napio/internal/factorio"
	"git.naknu.li/emily/napio/internal/gateway"
	"git.naknu.li/emily/napio/internal/supervisor"
)

// version is overridden at build time with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "napio: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "napio.yaml", "path to the configuration file")
		checkOnly   = flag.Bool("check", false, "validate the configuration and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("napio " + version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Printf("configuration is valid: %d server(s)\n", len(cfg.Servers))
		for _, s := range cfg.Servers {
			fmt.Printf("  %-20s %s/udp  backend=%s  timeout=%s\n",
				s.Name, s.UDPAddr(), s.Backend.Type, s.InactivityTimeout)
		}
		return nil
	}

	log := newLogger(cfg.LogLevel, cfg.LogFormat)
	log.Info("napio starting", "version", version, "servers", len(cfg.Servers))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	supervisors, err := buildSupervisors(ctx, cfg, log)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, s := range supervisors {
		wg.Add(1)
		go func(s *supervisor.Supervisor) {
			defer wg.Done()
			s.Run(ctx)
		}(s)
	}

	if cfg.API.Enabled {
		controllers := make([]api.Controller, 0, len(supervisors))
		for _, s := range supervisors {
			controllers = append(controllers, s)
		}
		srv := api.New(cfg.API, version, controllers, log)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.ListenAndServe(ctx); err != nil {
				log.Error("api server stopped", "error", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down; managed Factorio servers are left as they are")

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		log.Warn("shutdown timed out waiting for supervisors")
	}
	return nil
}

func buildSupervisors(ctx context.Context, cfg *config.Config, log *slog.Logger) ([]*supervisor.Supervisor, error) {
	var supervisors []*supervisor.Supervisor
	for _, sc := range cfg.Servers {
		serverLog := log.With("server", sc.Name)

		be, err := backend.New(sc.Backend, serverLog)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", sc.Name, err)
		}
		if d, ok := be.(*backend.Docker); ok {
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			for _, warning := range d.CheckConfiguration(checkCtx) {
				serverLog.Warn(warning)
			}
			cancel()
		}

		mon := factorio.NewRCONMonitor(sc.RCON.Address, sc.RCON.Password, 5*time.Second, serverLog)
		if !mon.Available() {
			serverLog.Warn("no rcon configured: napio cannot see players, so automatic " +
				"shutdown on inactivity is disabled for this server")
		}

		cidrs, err := gateway.ParseCIDRs(sc.Wake.AllowFrom)
		if err != nil {
			return nil, fmt.Errorf("server %s: wake.allow_from: %w", sc.Name, err)
		}
		filter := gateway.NewWakeFilter(
			sc.Wake.MinPacketSize, sc.Wake.Packets,
			sc.Wake.Window.Duration(), sc.Wake.Cooldown.Duration(), cidrs)

		var listener supervisor.Listener = gateway.NewListener(sc.UDPAddr(), filter, serverLog)
		if sc.Wake.Disabled {
			serverLog.Warn("automatic wake-up is disabled: this server can only be started through the API")
			listener = gateway.NewDisabledListener()
		}

		supervisors = append(supervisors, supervisor.New(sc, be, mon, listener, log))
	}
	if len(supervisors) == 0 {
		return nil, errors.New("no servers configured")
	}
	return supervisors, nil
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
