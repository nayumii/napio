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
	"git.naknu.li/emily/napio/internal/matchmaking"
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

		publisher, err := buildPublisher(sc, be, serverLog)
		if err != nil {
			return nil, fmt.Errorf("server %s: matchmaking: %w", sc.Name, err)
		}

		var listener supervisor.Listener = gateway.NewListener(sc.UDPAddr(), filter, serverLog)
		if sc.Wake.Disabled {
			serverLog.Warn("automatic wake-up is disabled: this server can only be started through the API")
			listener = gateway.NewDisabledListener()
		}

		supervisors = append(supervisors, supervisor.New(sc, be, mon, listener, publisher, log))
	}
	if len(supervisors) == 0 {
		return nil, errors.New("no servers configured")
	}
	return supervisors, nil
}

// buildPublisher wires the optional public-listing agent. It returns nil when
// matchmaking is disabled, which is the default.
func buildPublisher(sc config.Server, be backend.Backend, log *slog.Logger) (supervisor.Publisher, error) {
	mm := sc.Matchmaking
	if !mm.Enabled {
		return nil, nil
	}

	settings, err := matchmaking.LoadServerSettings(mm.SettingsPath())
	if err != nil {
		return nil, err
	}
	creds := matchmaking.Credentials{Username: mm.Username, Token: mm.Token}
	if creds.Username == "" {
		creds.Username = settings.Username
	}
	if creds.Token == "" {
		creds.Token = settings.Token
	}
	if creds.Username == "" || creds.Token == "" {
		return nil, errors.New("a factorio.com username and token are required: set matchmaking.username " +
			"and matchmaking.token, or put them in the server's server-settings.json")
	}
	if !settings.Visibility.Public {
		log.Warn("matchmaking is enabled but the server's own visibility.public is false, " +
			"so the listing will disappear when the server wakes up")
	}

	port, err := matchmaking.SplitHostPort(mm.PublicAddress)
	if err != nil {
		return nil, err
	}
	if port != sc.Port {
		log.Warn("matchmaking.public_address advertises a different port than napio guards; "+
			"make sure that is really what your port forwarding does",
			"advertised", port, "guarded", sc.Port)
	}

	logs, canReadLogs := be.(backend.LogSource)
	if !canReadLogs {
		return nil, fmt.Errorf("the %s backend cannot show the server log, so napio cannot learn the "+
			"exact Factorio build and will not advertise a guess", sc.Backend.Type)
	}

	build := func() (matchmaking.Game, error) {
		// Everything is re-read at publish time so an edited
		// server-settings.json or an upgraded server is picked up without a
		// napio restart.
		settings, err := matchmaking.LoadServerSettings(mm.SettingsPath())
		if err != nil {
			return matchmaking.Game{}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := logs.RecentLogs(ctx, 400)
		if err != nil {
			return matchmaking.Game{}, fmt.Errorf("reading the server log to determine its version: %w", err)
		}
		version, ok := matchmaking.ParseVersionBanner(raw)
		if !ok {
			return matchmaking.Game{}, errors.New("napio has not seen this server's version banner yet, " +
				"so it does not know which Factorio build to advertise; start the server once and it will be picked up")
		}
		mods, err := matchmaking.LoadMods(mm.ModsPath(), version.GameVersion)
		if err != nil {
			return matchmaking.Game{}, fmt.Errorf("reading the mod list (napio will not advertise an unknown mod set): %w", err)
		}
		return matchmaking.BuildGame(settings, version, mods, mm.PublicAddress, port, mm.Note()), nil
	}

	log.Info("public listing enabled for this server while it sleeps",
		"address", mm.PublicAddress, "username", creds.Username)
	return matchmaking.NewAgent(matchmaking.NewClient(mm.BaseURL), creds, build, log), nil
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
