# Configuration

napio reads a single YAML file, validates all of it at startup, and refuses to
run if anything is wrong. Check a file without starting anything:

```console
$ napio -config napio.yaml -check
configuration is valid: 1 server(s)
  my-factorio-server   0.0.0.0:34197/udp  backend=docker  timeout=10m0s
```

Unknown keys are errors, not typos silently ignored.

Durations accept either a duration string (`10m`, `90s`, `1h30m`) or a plain
number of seconds (`600`).

## Top level

| Key | Default | Meaning |
|---|---|---|
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `log_format` | `text` | `text` or `json` |
| `api` | disabled | See below |
| `defaults` | — | Values inherited by every server |
| `servers` | — | At least one required |

## `api`

```yaml
api:
  enabled: true
  listen: "127.0.0.1:8378"
  control: true
  token: "${NAPIO_API_TOKEN}"
```

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Serve the HTTP API |
| `listen` | `127.0.0.1:8378` | Bind address. Keep it on loopback unless it is behind a reverse proxy |
| `control` | `false` | Allow manual start/stop. Read-only when false |
| `token` | none | When set, required as `Authorization: Bearer <token>` |

## `defaults`

Any of `inactivity_timeout`, `start_timeout`, `stop_timeout`, `poll_interval`.
A server that sets the same key overrides the default.

## `servers[]`

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Unique identifier, used in logs and the API |
| `address` | `0.0.0.0` | Address napio binds while the server sleeps |
| `port` | required | The game's UDP port. Must be the port the server itself publishes |
| `inactivity_timeout` | `10m` | Shut down after this long with zero players |
| `start_timeout` | `5m` | Give up if the server is not ready by then |
| `stop_timeout` | `2m` | How long a graceful shutdown may take before escalating |
| `poll_interval` | `10s` | How often the player count is checked. Must be shorter than `inactivity_timeout` |
| `start_on_boot` | `false` | Start the server when napio starts, instead of waiting for a player |
| `rcon` | — | See below. Strongly recommended |
| `wake` | — | See below |
| `backend` | required | See below |

### `rcon`

```yaml
rcon:
  address: "127.0.0.1:27015"
  password: "${FACTORIO_RCON_PASSWORD}"
  failure_tolerance: 3
```

RCON is how napio knows the server has finished loading, how many players are
connected, and how to save before quitting.

**Without RCON, automatic shutdown is disabled.** napio will not guess that a
server is empty, because guessing wrong means kicking everybody off. Wake-on-
connect still works.

`failure_tolerance` is how many consecutive failed polls are tolerated before
the failure is surfaced in the status. The inactivity countdown is *paused*
while the player count is unknown — never advanced.

### `wake`

```yaml
wake:
  packets: 2
  window: 5s
  min_packet_size: 4
  cooldown: 30s
  allow_from:
    - 192.168.0.0/16
  disabled: false
```

napio never parses Factorio packets. It counts them: a real client retries its
connection request within a few seconds, a port scanner sends one datagram and
moves on. Defaults are fine for almost everyone.

| Key | Default | Meaning |
|---|---|---|
| `packets` | `2` | Datagrams from one source needed to wake the server |
| `window` | `5s` | ...within this window |
| `min_packet_size` | `4` | Ignore smaller datagrams |
| `cooldown` | `30s` | Ignore traffic for this long after a wake-up |
| `allow_from` | any | Optional CIDR allowlist |
| `disabled` | `false` | `true` = no wake-on-connect, API only |

Raising `packets` makes napio more resistant to being woken by scanners, at
the cost of needing a slightly longer connection attempt from the player.

### `backend`

#### Docker (recommended)

```yaml
backend:
  type: docker
  container: factorio
  # host: unix:///var/run/docker.sock
```

napio starts and stops a container **you** created. It never creates, removes
or reconfigures containers. `host` also accepts `tcp://host:port` (for example
a Docker socket proxy).

#### Process

```yaml
backend:
  type: process
  command:
    - /opt/factorio/bin/x64/factorio
    - --start-server
    - /opt/factorio/saves/world.zip
    - --server-settings
    - /opt/factorio/data/server-settings.json
  working_dir: /opt/factorio
  env:
    - "FOO=bar"
```

Runs Factorio as a child process. Note that a child process does not outlive
napio: if napio is restarted, it loses track of a running server. Prefer
Docker for unattended deployments.

## Secrets

Any `${VAR}` in `rcon.password` or `api.token` is replaced with the
environment variable of that name, and napio refuses to start if it is unset.
Keep passwords out of the config file:

```console
$ FACTORIO_RCON_PASSWORD=$(cat /srv/factorio/config/rconpw) napio -config napio.yaml
```

## Multiple servers

Each server is fully independent — its own port, container, timeout and state.
There is no routing to configure, because napio is never in the data path.

```yaml
servers:
  - name: main
    port: 34197
    rcon: {address: "127.0.0.1:27015", password: "${MAIN_RCON}"}
    backend: {type: docker, container: factorio-main}

  - name: modded
    port: 34198
    inactivity_timeout: 30m
    rcon: {address: "127.0.0.1:27016", password: "${MODDED_RCON}"}
    backend: {type: docker, container: factorio-modded}
```
