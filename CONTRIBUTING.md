# Contributing to napio

Thanks for considering it. napio is meant to be a small, dependable tool that
Factorio players can leave running for years, so the bar is "boring and
correct" rather than "clever".

## Development setup

```console
$ git clone https://git.naknu.li/emily/napio.git
$ cd napio
$ go build ./...
$ go test ./...
```

Go 1.25 or newer. There is nothing else to install.

```console
$ make build     # binary in ./napio
$ make test      # unit tests
$ make lint      # gofmt + go vet
$ make check     # all of the above
```

## Running against a real Factorio server

The unit tests are hermetic and use no network or Docker. There is also an
end-to-end test that drives a real container through a full wake/sleep cycle;
it is skipped unless you point it at one:

```console
$ export FACTORIO_DIR=$(mktemp -d)
$ docker run -d --name napio-test \
    -p 34197:34197/udp -p 127.0.0.1:27015:27015/tcp \
    -v "$FACTORIO_DIR:/factorio" \
    factoriotools/factorio:stable
# The first run generates a map; wait for "Starting RCON interface" in the log.

$ NAPIO_INTEGRATION_CONTAINER=napio-test \
  NAPIO_INTEGRATION_RCON_PASSWORD=$(cat "$FACTORIO_DIR/config/rconpw") \
  go test ./internal/integration/ -v -timeout 10m

$ docker rm -f napio-test && rm -rf "$FACTORIO_DIR"
```

The test stops the container, waits for napio to take the port, knocks on it,
and asserts the whole wake/sleep cycle — including that Factorio really is
answering RCON before napio calls it online.

## Project layout

```
cmd/napio            entry point, flags, wiring
internal/config      YAML loading and validation
internal/gateway     the UDP listener and the wake filter
internal/supervisor  the state machine — the core of napio
internal/backend     how a server is started and stopped (docker, process)
internal/factorio    the only Factorio-specific knowledge: rcon monitoring
internal/rcon        Source RCON client
internal/api         HTTP status and control
docs/                architecture and user documentation
```

The separation that matters: **backends know nothing about Factorio**, and
**Factorio knowledge does not know about backends**. Adding a backend
(systemd, a remote host, a hypervisor) should mean implementing three methods
and nothing else. If a change blurs that line, it probably belongs somewhere
different.

## Pull requests

- One logical change per PR, with a description of *why*.
- Tests for behaviour changes. The state machine especially: every failure
  mode in `docs/ARCHITECTURE.md` §7 should stay covered.
- `make check` passes.
- Documentation updated in the same PR when behaviour or configuration changes.
- Follow the surrounding style. Comments explain reasoning, not syntax.

## Things to be careful with

- **Never advance the inactivity countdown on an unknown player count.**
  Unknown is not zero. Shutting down a server full of people is the worst bug
  napio can have.
- **Never bind the UDP port while the Factorio server might still own it**, and
  never start a server while napio holds the port. The invariant is in
  `State.HoldsPort`.
- **Do not add game-protocol parsing.** It is undocumented, it changes between
  versions, and the failure mode is a server that never wakes. See the
  rejected alternatives in the architecture document.
- **Keep the dependency list at one.** If something needs a new dependency,
  say why in the PR.

## Adding a backend

1. Implement `backend.Backend` (`Describe`, `Start`, `Stop`, `IsRunning`).
2. Register it in `backend.New` and in the config validation.
3. Document it in `docs/CONFIGURATION.md`.
4. Add tests that cover "already running", "does not exist" and "fails to
   start".

## Releases

Semantic versioning. Until 1.0, the configuration format may change in minor
releases; when it does, the change goes in `CHANGELOG.md` with a migration
note.

## Code of conduct

Be decent to each other. See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
