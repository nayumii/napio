package matchmaking

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Backoff bounds for a failing service. napio never beats faster than a real
// Factorio server, and backs well off when the service is unhappy.
const (
	minBackoff = 30 * time.Second
	maxBackoff = 10 * time.Minute
)

// Status is what the API reports about a listing. It contains no secrets: no
// token, no listing secret, and no credentials in the error text.
type Status struct {
	Enabled bool   `json:"enabled"`
	Listed  bool   `json:"listed"`
	GameID  uint32 `json:"game_id,omitempty"`
	// LastHeartbeat is nil until the listing has been confirmed once.
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// GameBuilder assembles the listing at publish time, so a changed
// server-settings.json is picked up without restarting napio.
type GameBuilder func() (Game, error)

// Agent keeps a listing published while a server sleeps, and takes it down
// again the moment the real server is about to take over.
//
// Every operation is asynchronous and every failure is non-fatal: the agent
// exists to make a sleeping server findable, never to stand in the way of
// waking one.
type Agent struct {
	client *Client
	creds  Credentials
	build  GameBuilder
	log    *slog.Logger

	// Timings are fields rather than constants so the tests can exercise the
	// retry and republish paths without waiting half a minute.
	beatEvery  time.Duration
	backoffMin time.Duration

	mu       sync.Mutex
	listing  *Listing
	lastBeat time.Time
	lastErr  string
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewAgent builds an agent. It does not publish anything until Publish is called.
func NewAgent(client *Client, creds Credentials, build GameBuilder, log *slog.Logger) *Agent {
	return &Agent{
		client:     client,
		creds:      creds,
		build:      build,
		log:        log,
		beatEvery:  HeartbeatInterval,
		backoffMin: minBackoff,
	}
}

// Publish starts (or continues) advertising the sleeping server. It returns
// immediately; the work happens on the agent's own goroutine.
func (a *Agent) Publish() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return // already publishing
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.cancel, a.done = cancel, done
	go a.run(ctx, done)
}

// Withdraw removes the listing and stops heartbeating. It blocks briefly so
// the listing is gone before the real server registers its own, but never long
// enough to matter to a waiting player.
func (a *Agent) Withdraw() {
	a.mu.Lock()
	cancel, done, listing := a.cancel, a.done, a.listing
	a.cancel, a.done = nil, nil
	a.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.log.Warn("matchmaking agent did not stop promptly; continuing anyway")
	}

	if listing == nil {
		return
	}
	ctx, cancelRemove := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRemove()
	if err := a.client.Remove(ctx, *listing); err != nil {
		// Not fatal: the service drops listings that stop heartbeating, and
		// the real server is about to publish its own.
		a.log.Warn("could not withdraw the public listing; it will expire on its own",
			"game_id", listing.GameID, "error", err)
	} else {
		a.log.Info("withdrew the public listing", "game_id", listing.GameID)
	}
	a.mu.Lock()
	a.listing = nil
	a.mu.Unlock()
}

// Status reports what the API may show.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Status{Enabled: true, LastError: a.lastErr}
	if !a.lastBeat.IsZero() {
		beat := a.lastBeat
		s.LastHeartbeat = &beat
	}
	if a.listing != nil {
		s.Listed = true
		s.GameID = a.listing.GameID
	}
	return s
}

func (a *Agent) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	backoff := a.backoffMin
	for {
		if err := a.ensureListed(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			var httpErr *HTTPError
			if errors.As(err, &httpErr) && httpErr.Fatal() {
				a.setError("the matching service rejected napio's credentials: " + httpErr.Error())
				a.log.Error("public listing disabled: the factorio.com token was rejected",
					"status", httpErr.Status)
				return
			}
			a.setError(err.Error())
			a.log.Warn("could not publish the public listing; retrying",
				"error", err, "retry_in", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = a.backoffMin

		if !sleepCtx(ctx, a.beatEvery) {
			return
		}
		if err := a.beat(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			a.setError(err.Error())
			a.log.Warn("listing heartbeat failed; republishing", "error", err)
			a.mu.Lock()
			a.listing = nil // force a fresh create-game on the next pass
			a.mu.Unlock()
		}
	}
}

func (a *Agent) ensureListed(ctx context.Context) error {
	a.mu.Lock()
	listed := a.listing != nil
	a.mu.Unlock()
	if listed {
		return nil
	}

	game, err := a.build()
	if err != nil {
		return err
	}
	listing, err := a.client.Create(ctx, game, a.creds)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.listing, a.lastBeat, a.lastErr = &listing, time.Now(), ""
	a.mu.Unlock()
	a.log.Info("sleeping server is now listed in the public server browser",
		"game_id", listing.GameID, "name", game.Name, "address", game.HostAddress)
	return nil
}

func (a *Agent) beat(ctx context.Context) error {
	a.mu.Lock()
	listing := a.listing
	a.mu.Unlock()
	if listing == nil {
		return nil
	}
	if err := a.client.Heartbeat(ctx, *listing, nil); err != nil {
		return err
	}
	a.mu.Lock()
	a.lastBeat, a.lastErr = time.Now(), ""
	a.mu.Unlock()
	return nil
}

func (a *Agent) setError(message string) {
	a.mu.Lock()
	a.lastErr = message
	a.mu.Unlock()
}

// sleepCtx reports false if the context was cancelled while waiting.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
