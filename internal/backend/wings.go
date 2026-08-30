package backend

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Wings starts and stops a Factorio server managed by Pelican or Pterodactyl.
//
// It talks to the Wings daemon directly rather than to the panel's client API.
// That is not a shortcut, it is the only correct source: the panel caches
// /api/client/servers/{id}/resources for twenty seconds, and napio polls the
// server state every 250ms while waiting for a shutdown and every second while
// waiting for a start. A twenty-second-old "offline" arriving one second after
// a start would be read as "the server exited during startup" and abort a boot
// that was going perfectly well.
//
// Talking to Wings costs nothing in reach, either: napio has to run on the
// Wings node anyway, because the whole design rests on holding the game's UDP
// port and handing it back. The node's own token is on that same machine.
//
// Pelican and Pterodactyl expose identical routes here, so one backend serves
// both; config.BackendPterodactyl is an accepted spelling of the same thing.
type Wings struct {
	baseURL string
	uuid    string
	token   string
	logFile string

	// guardAddr and guardPort are the endpoint napio holds while the server
	// sleeps. They are kept only so CheckConfiguration can compare them with
	// the server's real allocation, which is the mistake most likely to make
	// a first deployment silently do nothing.
	guardAddr string
	guardPort int

	// Timings. Defaulted from the constants below and shortened by tests.
	startAck    time.Duration
	minGrace    time.Duration
	poll        time.Duration
	killReserve time.Duration

	client *http.Client
	log    *slog.Logger
}

// Wings process states, as reported by the daemon.
const (
	stateOffline  = "offline"
	stateStarting = "starting"
	stateRunning  = "running"
	stateStopping = "stopping"
)

// DefaultWingsLogFile is where the Factorio headless server writes its log,
// relative to the server's own directory. The standard Pelican/Pterodactyl
// Factorio egg does not pass --console-log, so Factorio uses its default.
const DefaultWingsLogFile = "factorio-current.log"

// Tunables. These are implementation details of talking to a local daemon,
// not deployment choices, so they are deliberately not configurable.
const (
	// stateAttempts is how many times a state read is retried before it is
	// reported as a failure. A single connection blip at napio startup would
	// otherwise put the supervisor straight into its error backoff, leaving
	// the server unwakeable for thirty seconds.
	stateAttempts   = 3
	stateRetryDelay = 250 * time.Millisecond

	// statePollInterval is how often the daemon is asked during the waits
	// below.
	statePollInterval = 250 * time.Millisecond

	// startAckTimeout is how long Start waits for the daemon to leave the
	// offline state. POST /power returns 202 and runs the action on another
	// goroutine, so the state has not moved yet when the response arrives.
	startAckTimeout = 15 * time.Second

	// killReserve is the part of the stop timeout held back for the escalation
	// to kill, so a server that ignores /quit is still stopped in time.
	killReserve = 20 * time.Second
	minGrace    = 5 * time.Second
)

// errTransient marks a failure worth retrying: the daemon was unreachable or
// answered 5xx. A 401, 403 or 404 is a deployment mistake and is never retried.
var errTransient = errors.New("transient wings failure")

// NewWings builds a Wings backend. baseURL is the daemon's own address, for
// example "https://node.example.com:8080".
func NewWings(baseURL, uuid, token, logFile string, skipVerify bool, guardAddr string, guardPort int, log *slog.Logger) (*Wings, error) {
	if baseURL == "" {
		return nil, errors.New("pelican backend: wings_url is required")
	}
	if uuid == "" {
		return nil, errors.New("pelican backend: uuid is required")
	}
	if token == "" {
		return nil, errors.New("pelican backend: token is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("pelican backend: wings_url %q must be a URL such as https://node.example.com:8080", baseURL)
	}
	if logFile == "" {
		logFile = DefaultWingsLogFile
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if skipVerify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	return &Wings{
		baseURL:     strings.TrimRight(parsed.String(), "/"),
		uuid:        uuid,
		token:       token,
		logFile:     logFile,
		guardAddr:   guardAddr,
		guardPort:   guardPort,
		startAck:    startAckTimeout,
		minGrace:    minGrace,
		poll:        statePollInterval,
		killReserve: killReserve,
		client:      client,
		log:         log,
	}, nil
}

// Describe deliberately omits the token: it is copied into logs and the HTTP
// status API.
func (w *Wings) Describe() string {
	host := w.baseURL
	if parsed, err := url.Parse(w.baseURL); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return fmt.Sprintf("pelican server %s on wings at %s", w.uuid, host)
}

// OwnsQuit reports that this backend performs Factorio's own clean quit.
//
// Wings' stop command for the Factorio egg is "/quit" on the server console,
// and Wings only skips its crash detection for a process it stopped itself.
// A server quit out of band over RCON therefore looks like a crash to Wings,
// which restarts it behind napio's back. See GracefulStopper.
func (w *Wings) OwnsQuit() bool { return true }

func (w *Wings) url(path string, query url.Values) string {
	u := w.baseURL + "/api/servers/" + w.uuid + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (w *Wings) do(ctx context.Context, method, path string, query url.Values, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.url(path, query), reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	// Without this the daemon is free to answer with something other than the
	// JSON these calls parse.
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: wings %s %s: %v", errTransient, method, path, err)
	}
	defer resp.Body.Close()
	// The same bound docker.go uses. A Factorio log can be large and only its
	// beginning is ever needed.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%w: wings %s %s: %v", errTransient, method, path, err)
	}
	return resp.StatusCode, payload, nil
}

// wingsServer is the part of GET /api/servers/{uuid} that napio uses.
type wingsServer struct {
	State         string `json:"state"`
	IsSuspended   bool   `json:"is_suspended"`
	Configuration struct {
		CrashDetectionEnabled bool `json:"crash_detection_enabled"`
		Allocations           struct {
			DefaultMapping *struct {
				IP   string `json:"ip"`
				Port int    `json:"port"`
			} `json:"default"`
		} `json:"allocations"`
	} `json:"configuration"`
}

func (w *Wings) fetch(ctx context.Context) (*wingsServer, error) {
	code, body, err := w.do(ctx, http.MethodGet, "", nil, nil)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		var out wingsServer
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("wings: decoding the server response: %w", err)
		}
		return &out, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("wings: no server with uuid %q exists on this node. "+
			"This must be the full UUID from the panel, not the short id in the panel URL", w.uuid)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("wings: the node rejected napio's token (%d). "+
			"Use the 'token' value from the node's Wings config.yml", code)
	default:
		if code >= 500 {
			return nil, fmt.Errorf("%w: wings returned %d: %s", errTransient, code, wingsMessage(body))
		}
		return nil, fmt.Errorf("wings: reading the server returned %d: %s", code, wingsMessage(body))
	}
}

// state reads the current process state, retrying failures that look transient.
func (w *Wings) state(ctx context.Context) (string, error) {
	var lastErr error
	for attempt := 0; attempt < stateAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(stateRetryDelay):
			}
		}
		info, err := w.fetch(ctx)
		if err == nil {
			return info.State, nil
		}
		lastErr = err
		if !errors.Is(err, errTransient) {
			break
		}
	}
	return "", lastErr
}

// IsRunning treats every state but "offline" as running, which is what the
// supervisor needs: "starting" must not read as a failed launch, and
// "stopping" must not read as a completed shutdown.
func (w *Wings) IsRunning(ctx context.Context) (bool, error) {
	state, err := w.state(ctx)
	if err != nil {
		return false, err
	}
	return state != stateOffline, nil
}

func (w *Wings) power(ctx context.Context, action string) error {
	body, err := json.Marshal(map[string]string{"action": action})
	if err != nil {
		return err
	}
	code, payload, err := w.do(ctx, http.MethodPost, "/power", nil, body)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusAccepted, http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("wings: no server with uuid %q exists on this node", w.uuid)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("wings: the node rejected napio's token (%d)", code)
	case http.StatusBadRequest:
		// This is what a suspended server answers to start.
		return fmt.Errorf("wings: %s was refused: %s", action, wingsMessage(payload))
	default:
		return fmt.Errorf("wings: %s returned %d: %s", action, code, wingsMessage(payload))
	}
}

// Start launches the server and waits for the daemon to acknowledge it.
//
// POST /power returns 202 immediately and performs the action on another
// goroutine, so a bare 202 means nothing has happened yet. Waiting for the
// state to leave "offline" is also the only place a launch failure becomes
// visible: Wings reports asynchronous power errors over its websocket, never
// in this response.
func (w *Wings) Start(ctx context.Context) error {
	state, err := w.state(ctx)
	if err != nil {
		return err
	}
	if state != stateOffline {
		return nil // already running, starting or still stopping
	}

	if err := w.power(ctx, "start"); err != nil {
		return err
	}

	if _, err := w.await(ctx, w.startAck, func(s string) bool { return s != stateOffline }); err != nil {
		if errors.Is(err, errWaitTimeout) {
			return fmt.Errorf("wings accepted the start but %s is still offline %s later. "+
				"The daemon reports launch failures only over its websocket, so check the server's "+
				"console in the panel: a missing image, a full disk or a suspended server all look like this",
				w.uuid, w.startAck)
		}
		return err
	}
	return nil
}

// Stop asks Wings to stop the server, escalating to kill if it does not go.
//
// Wings' own stop waits up to ten minutes and never escalates on its own, so
// the escalation has to happen here for the Backend contract to hold.
func (w *Wings) Stop(ctx context.Context, timeout time.Duration) error {
	state, err := w.state(ctx)
	if err == nil && state == stateOffline {
		return nil
	}

	if err := w.power(ctx, "stop"); err != nil {
		return err
	}

	grace := timeout - w.killReserve
	if grace < w.minGrace {
		grace = w.minGrace
	}
	if _, err := w.await(ctx, grace, isOffline); err == nil {
		return nil
	} else if !errors.Is(err, errWaitTimeout) {
		return err
	}

	// The graceful stop did not take. Escalate on a context of its own: the
	// caller's deadline is the whole stop budget, and refusing to kill because
	// it has run out would leave the server running and the port held.
	w.log.Warn("server did not stop in time, asking wings to kill it; unsaved progress since the last save may be lost",
		"waited", grace)
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.killReserve)
	defer cancel()
	if err := w.power(killCtx, "kill"); err != nil {
		return fmt.Errorf("wings: escalating to kill: %w", err)
	}
	if _, err := w.await(killCtx, w.killReserve, isOffline); err != nil {
		return fmt.Errorf("wings: the server is still running after a kill: %w", err)
	}
	return nil
}

func isOffline(state string) bool { return state == stateOffline }

// errWaitTimeout distinguishes "the state never arrived" from "the daemon
// could not be reached", which are different problems with different fixes.
var errWaitTimeout = errors.New("timed out waiting for the server state")

// await polls the daemon until pred accepts the state, or budget elapses.
func (w *Wings) await(ctx context.Context, budget time.Duration, pred func(string) bool) (string, error) {
	deadline := time.Now().Add(budget)
	for {
		state, err := w.state(ctx)
		if err != nil {
			return "", err
		}
		if pred(state) {
			return state, nil
		}
		if time.Now().After(deadline) {
			return state, fmt.Errorf("%w after %s (state is %q)", errWaitTimeout, budget, state)
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-time.After(w.poll):
		}
	}
}

// RecentLogs returns the beginning of the Factorio log, which is where the
// version banner is.
//
// This reads the log file rather than the daemon's own /logs endpoint on
// purpose. That endpoint returns at most the last hundred console lines, and
// the banner napio needs is the first line of a run, so on any server that has
// been played for a while it is long gone from the tail.
//
// Factorio truncates factorio-current.log at every launch (moving the previous
// one aside), so its first lines always describe the build that is installed
// now, even while the server is asleep.
func (w *Wings) RecentLogs(ctx context.Context, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	q := url.Values{"file": {w.logFile}}
	code, body, err := w.do(ctx, http.MethodGet, "/files/contents", q, nil)
	if err != nil {
		return "", err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("wings: the server has no file %q. Set backend.log_file to the "+
			"Factorio log's path relative to the server's own directory", w.logFile)
	default:
		return "", fmt.Errorf("wings: reading %q returned %d: %s", w.logFile, code, wingsMessage(body))
	}

	// Keep the head, not the tail: the banner is written at startup.
	text := string(body)
	if split := strings.SplitAfterN(text, "\n", lines+1); len(split) > lines {
		text = strings.Join(split[:lines], "")
	}
	return text, nil
}

// CheckConfiguration reports deployment mistakes that would leave napio
// running happily while doing nothing at all. It is advisory: a warning, never
// a hard failure.
func (w *Wings) CheckConfiguration(ctx context.Context) []string {
	info, err := w.fetch(ctx)
	if err != nil {
		return []string{err.Error()}
	}

	var warnings []string
	if info.IsSuspended {
		warnings = append(warnings, fmt.Sprintf(
			"server %s is suspended in the panel, so napio will not be able to start it", w.uuid))
	}

	// The single most valuable check in this backend. napio guards a UDP
	// endpoint; if that is not the endpoint the panel allocated, players wake
	// nothing and napio reports itself perfectly healthy while doing so.
	def := info.Configuration.Allocations.DefaultMapping
	if def == nil {
		warnings = append(warnings, "this server has no default allocation, so napio cannot confirm "+
			"that it is guarding the port players actually connect to")
		return warnings
	}
	switch {
	case def.Port != w.guardPort:
		warnings = append(warnings, fmt.Sprintf(
			"napio guards UDP port %d but this server's default allocation is %s:%d; "+
				"players connect to the allocation, so set servers[].port to %d",
			w.guardPort, def.IP, def.Port, def.Port))
	case w.guardAddr == "0.0.0.0" || def.IP == w.guardAddr || def.IP == "":
		// Guarding every address covers whichever one the allocation uses.
	case def.IP == "0.0.0.0":
		warnings = append(warnings, fmt.Sprintf(
			"this server's allocation accepts players on every address of the node, but napio only "+
				"guards %s; a player reaching the node on any other address will not wake it. "+
				"Set servers[].address to 0.0.0.0", w.guardAddr))
	default:
		warnings = append(warnings, fmt.Sprintf(
			"napio binds %s but this server's default allocation is %s; a player connecting to %s "+
				"will not wake it. Set servers[].address to %s, or to 0.0.0.0",
			w.guardAddr, def.IP, def.IP, def.IP))
	}
	return warnings
}

// wingsMessage pulls the daemon's own explanation out of an error response.
func wingsMessage(body []byte) string {
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.Error != "" {
		return out.Error
	}
	return strings.TrimSpace(string(body))
}
