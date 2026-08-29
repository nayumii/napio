# napio — Architecture

> Status: **validated** (see [Validation result](#validation-result)).
> Target: Factorio 2.0.x headless servers. Nothing else.

napio keeps a Factorio server asleep until somebody actually tries to play, and puts
it back to sleep when the last player leaves. The machine stays up; the Factorio
process/container does not.

---

## 1. What we are working against

Everything below was checked against a real `factoriotools/factorio:stable`
container running **Factorio 2.0.77 (headless, space-age)**, not assumed.

| Fact | Evidence |
|---|---|
| Factorio multiplayer is **UDP only** (default `34197/udp`). There is no TCP game port. | `Info UDPSocket.cpp:38: Opening socket at (IP ADDR:({0.0.0.0:34197}))` / `Hosting game at ...`; official wiki. |
| The protocol is a **custom reliable layer over UDP**, undocumented by Wube. The community reverse-engineering effort is partial. | Factorio wiki; forum thread [127546](https://forums.factorio.com/viewtopic.php?t=127546) where a user asked for exactly this and was told the protocol is not documented. |
| The server discovers its **own public IP:port** by asking `pingpong1..4`, then registers with the matching server **from its own UDP socket**. | `Info ServerRouter.cpp:547: Own address is IP ADDR:({<public-ip>:34197}) (confirmed by pingpong1)` |
| Public-list registration is done **by the Factorio process itself** using the factorio.com token, and fails loudly without one. | `Error ServerMultiplayerManager.cpp:743: Matching server connection failed: Error when creating server game: Missing token.` |
| **NAT punching** works by the game server sending a packet *directly to the client* from that same socket, coordinated by the matching server. | [FFF-143](https://factorio.com/blog/post/fff-143) — "a specialised version of STUN and ICE adapted to run over Factorio protocol". |
| The matchmaking endpoints that create/keep a public listing (`create-game`, `post-game-heartbeat`, `remove-game`) are **undocumented** — the wiki and the community API docs both say "TODO". | [wiki.factorio.com/Matchmaking_API](https://wiki.factorio.com/Matchmaking_API), [artentus API doc](https://artentus.github.io/FactorioApiDoc/matchmaking-api/) |
| **RCON** exists (Source RCON over TCP) and adds two console-only commands. | `/help` over RCON returns: `/quit - Terminates the server cleanly.` `/save <name> - Saves the current game with the specified name.` |
| **RCON starts listening only after the save is loaded and the server is `InGame`.** | Cold start log order: `changing state from(CreatingGame) to(InGame)` at `3.856`, then `Starting RCON interface` at `7.089`. |
| `/quit` over RCON exits cleanly (exit code 0, ~2 s) but **does not save**. | No `Saving game` line between `/quit` and `Goodbye` in the log. |
| `SIGTERM` (`docker stop`) **does** save, then exits. | `Quitting: signal.` → `Saving map as /factorio/saves/_autosave1.zip` → `Goodbye`, exit code 143. |
| `/players online` returns a parseable, stable string. | `'Online players (0):\n'` |
| With Docker's userland proxy enabled, `docker start` **fails** if something else holds the published host port; the port is free **immediately** (measured 0.00 s) after `docker stop` returns. | `docker start` while holding `0.0.0.0:34197/udp` → `driver failed programming external connectivity`; after release → success. |
| Warm restart of a small save reached RCON-ready in **1.2 s**; the cold run (map generation + mod load) took **~7 s** to RCON-ready. Real savegames are slower — seconds to a minute. | Measured. |
| The Factorio client gives up on an unresponsive server after roughly **10 s**. | Community reports / client logs. |
| Factorio's RCON auth replies with exactly **one** packet: type 2, carrying the request id on success and `-1` on failure (no empty `RESPONSE_VALUE` prelude). | Captured from a live server: `packet id=7 type=2` / `packet id=-1 type=2`. |
| A headless server for a **public** game needs to reach `auth.factorio.com` at startup and fails hard without it. | `Failed to reach auth server. Code: 520` → `InitializationFailed` → exit 1, reproduced by taking DNS away from the container. |

Two of these facts decide the entire design:

1. **The game server's own UDP socket is its identity.** Public-list registration,
   pingpong address discovery and NAT punching all use it, and the client is told
   to expect packets *from that exact address:port*.
2. **The protocol is undocumented.** Anything that requires napio to parse or forge
   Factorio packets is a permanent maintenance liability that breaks on game updates.

---

## 2. Proposed architecture — "port handover"

napio is a small supervisor that **owns the public UDP port only while the server is
asleep**, and gets out of the way completely while it is awake.

```
                    ┌──────────────────────── napio (always running) ───────────────────────┐
                    │                                                                        │
   Player           │   listener        supervisor            monitor          backend       │
     │              │  (UDP socket)   (state machine)    (RCON client)    (Docker/…)         │
     │  UDP :34197  │       │                │                 │               │            │
     ├──────────────┼──────▶│  wake trigger  │                 │               │            │
     │              │       ├───────────────▶│  close socket   │               │            │
     │              │       │                ├─────────────────┼──────────────▶│ start      │
     │  (client      )      │                │                 │               │            │
     │   retries    │       │                │  poll RCON ────▶│  ready?       │            │
     │              │       │                │                 │               │            │
     ├──────────────┼───────┼────────────────┼─── direct to Factorio ──────────▶│ :34197    │
     │  joins       │       │                │                 │               │            │
     │              │       │                │  0 players 10m  │               │            │
     │              │       │                ├─── /save,/quit ─┼──────────────▶│ stop       │
     │              │       │◀── rebind ─────┤                 │               │            │
                    └────────────────────────────────────────────────────────────────────────┘
```

**While ONLINE, napio touches no game traffic at all.** The Factorio process binds
the real public port and talks to clients, to pingpong and to the matching server
exactly as it would without napio. There is nothing for napio to get wrong on the
hot path — no relay, no parsing, no rewriting, no added latency, no MTU concerns,
and no risk to NAT punching or public-list visibility.

**While OFFLINE, napio binds that same port** and listens for datagrams. It never
answers them; it only uses their arrival as a doorbell. Holding the socket (rather
than sniffing) also means the kernel does not emit ICMP *port unreachable*, so the
client sits in its normal "connecting" retry loop instead of failing instantly.

### Components

| Component | Responsibility | Factorio-specific? |
|---|---|---|
| `config` | Load + validate YAML, defaults, no hidden state | no |
| `supervisor` | One per configured server. Owns the state machine and the socket-ownership invariant. | no |
| `listener` | Bind/unbind the UDP port, apply the wake filter | no |
| `backend` | `Start` / `Stop` / `IsRunning` — Docker today, systemd/process later | no |
| `rcon` | Source RCON client: readiness probe, player count, `/save`, `/quit` | yes |
| `api` | Read-only status + manual start/stop over HTTP | no |

Readiness detection and player counting live in the **Factorio-aware monitor
layer**, deliberately *not* in the backend interface. That is what keeps a second
backend (systemd, plain process) a ~100-line file instead of a redesign.

### State machine

```
                 wake trigger / API start
      ┌────────────────────────────────────────────┐
      ▼                                            │
  ┌────────┐   release socket   ┌──────────┐  ready ┌────────┐
  │OFFLINE │ ─────────────────▶ │ STARTING │ ─────▶ │ ONLINE │
  └────────┘                    └──────────┘        └────────┘
      ▲                              │ fail              │ 0 players for T
      │                              ▼                   ▼
      │  bind socket             ┌───────┐  backoff  ┌──────────┐
      └──────────────────────────│ ERROR │◀──────────│ STOPPING │
                                 └───────┘           └──────────┘
                                     │ manual start / backoff elapsed
                                     └──────────────▶ OFFLINE
```

**Socket-ownership invariant:** napio holds the UDP listener **if and only if** the
state is `OFFLINE` or `ERROR`, and only the supervisor goroutine may bind or unbind
it. This single rule removes the entire class of "napio and Factorio both want port
34197" races, and it is enforced by the state transitions rather than by locking
discipline spread across the codebase.

---

## 3. How wake-on-connect works

1. Server is `OFFLINE`; napio holds `0.0.0.0:34197/udp`.
2. A player clicks Connect. The client sends its connection request and retries for
   ~10 s. napio receives the datagrams.
3. The **wake filter** decides whether this is a player or noise (see below).
4. napio → `STARTING`: closes the listener *first* (mandatory — `docker start` fails
   while the port is held; measured), then calls `backend.Start()`.
5. napio polls RCON until it authenticates and answers `/players online`. That is
   the readiness signal: RCON only comes up after the save is loaded.
6. Server is `ONLINE`. From here the client talks to Factorio directly.

**Honest UX:** a cold start is usually slower than the client's ~10 s patience. The
realistic experience is: *first attempt wakes the server and times out; the second
attempt, ~20–40 s later, joins.* Keeping the connection alive across the wake is
not possible without forging protocol packets, which we explicitly refuse to do
(§9). napio's job is to make the second attempt always work, and to make the wait
predictable. Where the save is small and the container is warm, the client's own
retry window can cover the whole start and the first attempt succeeds.

### The wake filter (no protocol parsing)

A naked UDP port on the internet gets scanned. Waking a server for a scanner is the
one failure mode that makes the whole project annoying rather than useful. napio
distinguishes a player from noise **without understanding a single byte of the
protocol**:

* **Minimum datagram size** — drops single-byte probes.
* **≥ N datagrams from the same source IP within a window** (default 2 within 5 s) —
  a real client retries its connection request; a scanner sends one packet and moves on.
* **Post-attempt cooldown** — a failed start cannot be hot-looped by traffic.
* **Optional `allow_from` CIDR list** — for people who want it locked down.

This costs ~1–2 s of an already ~30 s wake, and it cannot rot when Wube changes the
protocol, because there is nothing version-specific in it.

---

## 4. How player detection works

RCON, polled every `poll_interval` (default 10 s) while `ONLINE`:

```
/players online   →   "Online players (2):\n  Alice (online)\n  Bob (online)\n"
```

The count is parsed from the header's parentheses; names are parsed best-effort for
the API and are never used for control decisions. napio uses **plain console
commands only** — never `/c` or `silent-command`, which would flag the save as
command-modified and disable achievements on a community server.

If RCON is not configured, napio falls back to reading the backend's log stream for
join/leave lines. This is documented as the degraded mode: it is enough to keep the
inactivity timer honest, but RCON is strongly recommended and is required for
`/save`-before-`/quit`.

The inactivity countdown starts when a poll observes 0 players and is **reset by any
observation of ≥ 1 player**, including one that arrives mid-countdown. There is no
point of no return: a player joining during the countdown simply cancels it.

---

## 5. How graceful shutdown works

Empirically, `/quit` does **not** save and `SIGTERM` **does**. The ladder therefore is:

1. `/save` over RCON → wait for the command to return (measured: ~50 ms on a small map).
2. `/quit` over RCON → wait for the process to exit (measured: ~2 s, exit code 0).
3. If still running after `stop_timeout`: `backend.Stop()` → SIGTERM (which saves again).
4. If still running: SIGKILL, logged loudly as data loss risk.
5. Confirm the process is gone, then rebind the listener → `OFFLINE`.

Step 5 is gated on *confirmed exit*, not on the stop call returning, because the
listener must never be bound while Factorio still owns the port.

The last rung of that ladder is a data-loss risk: a SIGKILL delivered while a
very large map is still being written can truncate the save. `stop_timeout`
(default two minutes) is the knob for this — raise it for maps big enough that
saving takes real time, and napio will simply wait longer before escalating.

---

## 6. How multiple servers are routed

They are not routed. Each configured server has its own public UDP port and its own
independent supervisor, listener, RCON client and backend handle. There is no
demultiplexer, no shared socket and no protocol inspection — which is only possible
*because* we do not proxy. Two servers on the same port would be impossible for
Factorio anyway; two servers on two ports need no routing at all.

Servers are fully independent: one crash-looping server does not affect another, and
each has its own timeout, backend and status.

---

## 7. Failure scenarios and recovery

| Scenario | Detection | Response |
|---|---|---|
| Backend start returns an error | `backend.Start()` error | → `ERROR`, rebind listener, exponential backoff (30 s → 5 min), surfaced in API |
| Start succeeds but never becomes ready | RCON probe times out after `start_timeout` (default 300 s) | Stop the backend, → `ERROR`, backoff |
| Server crashes while `ONLINE` | `IsRunning()` false / RCON connection refused twice in a row | → `OFFLINE` + rebind (not `ERROR` — the next player wakes it). Repeated crashes within a window → `ERROR` + backoff |
| Player joins during `STARTING` | n/a | Nothing to do — the client is already retrying at the real server |
| Player joins during `STOPPING` | n/a | The stop is allowed to finish, then a start is queued. Cancelling `/quit` mid-flight is not reliable, so we do not try |
| `/quit` fails or hangs | Process still alive after `stop_timeout` | Escalate the ladder (§5) |
| RCON temporarily unreachable | Probe error while `ONLINE` | Tolerated for `rcon_failure_tolerance` consecutive polls; the inactivity timer is **paused**, never advanced on unknown player count |
| napio restarts | Startup reconciliation | Query each backend: running → `ONLINE` (no bind), stopped → `OFFLINE` (bind). napio holds no persistent state |
| Port still held after stop | Bind returns `EADDRINUSE` | Retry with backoff, log, stay out of `OFFLINE` until the bind succeeds |
| Transient network failure at startup (Factorio cannot reach `auth.factorio.com`, DNS down, matching server unreachable) | The server exits during startup; readiness never arrives | Treated as a startup failure: stop, `ERROR`, backoff. The next player's attempt after the backoff retries it, which is usually all a transient fault needs |
| Scanner / port-scan traffic | Wake filter (§3) | Ignored |

napio keeps **no database and no state file**. Everything it needs is observable
from the backend and RCON at any moment, which is what makes restart-safety a
five-line reconciliation instead of a crash-recovery protocol.

---

## 8. Docker integration

The Docker backend talks to `/var/run/docker.sock` over three endpoints
(`GET /containers/{id}/json`, `POST /containers/{id}/start`, `POST /containers/{id}/stop`)
using the standard library over a unix-socket transport. The official Docker SDK
pulls in a very large dependency tree for what is three HTTP calls, and this project
is meant to be small enough to audit.

napio **manages an existing container** (`docker start` / `docker stop`); it does not
create containers, own volumes or template compose files. Your Factorio deployment
stays yours — napio only decides *when* it runs.

Requirements, all consequences of the handover design:

* The container publishes the game port on the host (`-p 34197:34197/udp`), and napio
  must be able to bind that same host port. In practice napio runs on the host, or in
  a container with `network_mode: host`.
* The container must **not** have `restart: always` / `unless-stopped` — that fights
  napio for control. Use `restart: "no"`.
* RCON should be published on loopback only (`-p 127.0.0.1:27015:27015/tcp`).
* With Docker's userland proxy enabled (the default), the host port is bound by
  `docker-proxy` for the container's whole lifetime; with it disabled, Docker uses
  iptables DNAT and never binds the port. napio's release-before-start ordering is
  correct in both modes, and was verified against the userland-proxy mode where the
  conflict is real.

Other backends (systemd unit, plain subprocess) implement the same three-method
interface and are additive.

---

## 9. Security considerations

* **The UDP listener is a doorbell, not a parser.** napio never interprets the
  contents of a game packet, so a malformed or hostile datagram has no attack surface
  beyond "counts toward the wake filter". No allocation is sized from packet data.
* **Wake amplification / DoS.** An attacker who can reach the port can force a start.
  Mitigations: the wake filter's repeat requirement, per-server cooldown, optional
  `allow_from` CIDR allowlist, and the fact that a wake costs the attacker nothing but
  gains them nothing — the server still requires normal Factorio auth to actually join.
* **RCON is a remote code path.** Bind it to loopback, use a strong password, keep it
  off the public interface. napio reads the password from the config file or from an
  environment variable (`${VAR}` expansion) so it need not be committed.
* **The Docker socket is root-equivalent.** Documented explicitly. Users who mind can
  use a socket proxy restricted to the three endpoints napio needs, or the process
  backend.
* **The HTTP API binds `127.0.0.1` by default**, is read-only unless control is
  enabled, and uses a bearer token when exposed. It never returns RCON passwords or
  factorio.com tokens.
* **No credentials leave the machine.** napio does not talk to factorio.com at all.

---

## 10. Limitations imposed by Factorio itself

1. **The first connection attempt will usually time out.** ~10 s of client patience vs.
   a start that takes longer. The player reconnects and joins. This is inherent to
   UDP + an undocumented protocol; no design short of forging packets avoids it.
2. **A sleeping server is not in the public server browser.** The listing is
   heartbeat-driven by the running Factorio process, so it disappears when the process
   does. While the server is awake, public-list visibility is *completely native* —
   which is precisely what the handover design buys, and what a proxy design would
   put at risk. Players wake a sleeping server via direct connect (the client
   remembers the last address). napio could in principle publish the listing itself
   while the server sleeps; the protocol for doing so has been captured and
   documented in [MATCHMAKING.md](MATCHMAKING.md), and §11 explains what still has
   to be validated before it could ship.
3. **No "server is starting" MOTD.** Factorio has no documented query/status packet we
   can safely answer, and the deny reasons in the handshake are an internal enum, not
   free text. napio surfaces status through its HTTP API instead.
4. **Save-on-quit must be explicit.** `/quit` does not save; napio always `/save`s first.
5. **Version coupling of readiness is behavioural, not protocol-level** — we depend on
   "RCON comes up after the map is loaded", which is a much weaker and more stable
   assumption than any packet layout.

---

## 11. Alternatives considered and rejected

**UDP relay / proxy in front of the server (the lazymc model).**
Rejected. Factorio's matching-server registration, pingpong address discovery and NAT
punching are all performed by the game server's *own* socket, and the client is told
to expect traffic from that exact address:port ([FFF-143](https://factorio.com/blog/post/fff-143)).
Putting a relay in front changes that identity, which risks both public-list
visibility (requirement 7) and the NAT-punch path that FFF-143 itself only claims
~70 % success for. It also adds a hop to a lockstep game, and buys nothing:
a relay still cannot hold the client past its ~10 s timeout, because it cannot speak
the protocol.

**Parsing the handshake to identify a genuine join (message-type byte heuristics).**
Rejected for v1. The protocol is undocumented and version-unstable, and the failure
mode is the worst available: a real player's packet misclassified as noise means the
server never wakes. The repeat-datagram heuristic (§3) achieves the same goal with no
version coupling.

**Forging a "server is starting" reply / a fake handshake denial.**
Rejected. Requires reimplementing an undocumented protocol, breaks on game updates,
and risks confusing real clients.

**pcap / raw-socket traffic sniffing (the Lazytainer model).**
Rejected. Requires `CAP_NET_RAW`, complicates containerised deployment, and leaves the
port unbound so the kernel answers with ICMP *port unreachable* — which can abort the
client's retry loop instead of letting it ride out the start. Binding the socket is
both safer and better behaved.

**Registering a placeholder entry in the public server list while asleep.**
Not rejected on principle any more, but **not implemented**: the mechanism is now
known and the blocker has moved. The full protocol was captured from a real
2.0.77 server — `create-game`, a `post-game-heartbeat` patch every 30 s, and
`remove-game` on exit — and is written up in [MATCHMAKING.md](MATCHMAKING.md)
with a reproducible lab rig in `hack/matchmaking-lab/`.

What stops it is validation, not knowledge. Three questions can only be answered
with the real matching service and a real Factorio client: whether the service
accepts a listing from something that is not a running game server, whether the
client's Join path needs the listed endpoint to speak Factorio's protocol before
napio can wake anything, and where an honest `game_time_elapsed` would come from
while the server is off. Until those are answered, shipping the feature would
mean advertising servers that may not be joinable — so napio does not, and no
release claims public browser support. The runbook that settles it is in
[MATCHMAKING.md](MATCHMAKING.md#validation-runbook).

**systemd socket activation.**
Rejected as *the* mechanism, though it is the closest existing approach and was
suggested in the forum thread. It cannot express readiness detection, player-count
polling, graceful `/save` + `/quit`, multi-server status or an API, and it ties the
project to systemd. napio's listener is the same idea, generalised and portable.

**A second, lightweight "waiting room" Factorio instance.**
Rejected. Doubles the resident footprint (defeating the purpose on a NAS or Pi),
requires firewall rewriting to hand players over, and cannot migrate a connected
client to the real server.

**Keeping the server running and relying on auto-pause.**
Rejected as a non-solution: auto-pause saves CPU but not the RAM and disk that make
people want this in the first place.

---

## Validation result

**Architecture validated.**

It has also been executed end to end: an automated test drives a real
`factoriotools/factorio:stable` container through the full cycle — napio holding
`0.0.0.0:34197/udp` while asleep, a UDP connection attempt triggering the wake,
the port handed over, RCON readiness reached, the inactivity timeout expiring,
`/save` + `/quit`, and the port taken back. It is in
`internal/integration/`, and it passed against Factorio 2.0.77 with a warm start
of 1.2 s and a graceful shutdown of 0.5 s.

The design rests on behaviour that was measured against a live Factorio 2.0.77
headless server, not inferred: the UDP-only game port, the RCON-after-load readiness
ordering, the `/quit`-does-not-save and `SIGTERM`-does-save asymmetry, the
`/players online` output format, and the Docker port-handover conflict and its
resolution. The one design risk that mattered — whether a gateway in the data path
would break public-list visibility and NAT punching — is avoided entirely rather than
mitigated, because napio is not in the data path while the server is online.

The two accepted limitations (first connect attempt times out; a sleeping server is
not listed in the public browser) are inherent to Factorio's networking model, are
documented for users up front, and do not compromise the goal: while the server is
awake it is a completely ordinary, fully visible, fully native Factorio server.
