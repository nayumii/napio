# Changelog

All notable changes to this project are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Pelican and Pterodactyl backend (`backend.type: pelican`, or `pterodactyl`
  for the same thing). napio starts and stops a server the panel already
  manages; it never creates, deletes or reconfigures one.
- napio checks the server's default allocation against the UDP endpoint it
  guards at startup, and warns when they disagree. Guarding the wrong port is
  the one mistake that leaves napio looking perfectly healthy while waking
  nothing at all.
- `docs/DEPLOYMENT.md` gained a Pelican/Pterodactyl walkthrough, including
  where the three required values live and why napio has to run on the Wings
  node.

### Notes on that backend

- napio talks to the **Wings daemon**, not to the panel's client API. The panel
  caches the server state for twenty seconds, which is long enough for napio to
  read a booting server as one that failed to start and abort it. Wings answers
  from the live container state.
- napio does not send `/quit` over RCON on this backend. Wings treats a process
  that exits without its say-so as a crash and restarts it, so napio saves the
  map over RCON and then asks Wings to stop the server, which sends the egg's
  own `/quit`. This is the `GracefulStopper` capability in `internal/backend`.
- Because the stock Factorio egg's stop command is `/quit`, which does not
  save, RCON is worth configuring here even though Wings can stop the server
  without it.

### Changed

- `backend.New` takes the whole `config.Server` rather than just its `backend`
  section, so a backend can compare the endpoint napio guards with the one the
  platform publishes.
- `factorio.Monitor` gained `Save`, for shutting down through a backend that
  performs the quit itself.

## [0.2.0] — 2026-08-29

### Added

- Optional public server-browser listing for sleeping servers
  (`matchmaking.enabled`, off by default). napio publishes the listing that the
  stopped Factorio process can no longer publish, heartbeats it at Factorio's
  own 30-second cadence, and withdraws it the moment the real server takes
  over. Every advertised field is read from the server's own configuration,
  mods and log; `game_time_elapsed`, which napio cannot observe while the
  server is off, is not sent rather than guessed.
- `docs/MATCHMAKING.md`: the matchmaking protocol, captured from a real
  Factorio 2.0.77 server and verified against the live service, plus what
  remains unverified.
- `hack/matchmaking-lab/`: a reproducible rig for re-capturing that protocol
  against future Factorio versions.
- The HTTP API reports listing state (`listing`), with no credentials in it.
- Debug-level logging of every datagram the wake listener receives, so "did any
  traffic arrive?" is answerable without tcpdump.

Verified end to end with a real Factorio 2.1.17 client: the sleeping server was
found by name in the public server browser, and clicking Join woke it and let
the player in about thirteen seconds later, without a retry. The client's
3-byte browser probes are filtered by the default `wake.min_packet_size`, so
browsing a list of sleeping servers wakes none of them.

### Fixed

- The status snapshot could go stale between state changes; it is now refreshed
  on every poll.

## [0.1.0] — 2026-08-29

First release. Small on purpose.

### Added

- Wake-on-connect for Factorio dedicated servers, using port handover rather
  than a proxy: napio holds the game's UDP port only while the server is
  asleep, and is completely out of the network path while it runs.
- Behavioural wake filter (repeated datagrams from one source) that
  distinguishes a retrying Factorio client from a port scanner without
  parsing any game protocol.
- Readiness detection via RCON, which the Factorio server only starts once the
  save has loaded.
- Player detection via `/players online`, using plain console commands so
  achievements are never disabled.
- Graceful shutdown ladder: `/save`, `/quit`, then SIGTERM, then SIGKILL.
- Configurable inactivity timeout per server, paused rather than advanced when
  the player count is unknown.
- Multiple independent servers, each with its own port, backend, timeout and
  status.
- Docker backend (three daemon endpoints, no Docker SDK dependency) and a
  process backend.
- Crash, startup-failure and shutdown-failure handling with exponential
  backoff, and adoption of servers that are already running.
- Optional HTTP API for status and manual start/stop, designed as the surface
  a future web UI can be built on.
- Documentation: architecture and validation, configuration, deployment, API,
  troubleshooting, security.
