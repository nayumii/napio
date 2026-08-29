# HTTP API

Disabled by default. Enable it in the configuration:

```yaml
api:
  enabled: true
  listen: "127.0.0.1:8378"
  control: true                 # required for start/stop
  token: "${NAPIO_API_TOKEN}"   # optional bearer token
```

The API is deliberately small: it is the surface a web UI would be built on,
so it exposes state and two actions and nothing else. Secrets (RCON passwords,
tokens) are never included in a response.

## `GET /healthz`

Unauthenticated, for container health checks.

```json
{"status": "ok", "version": "0.1.0"}
```

## `GET /api/v1/servers`

```json
{
  "servers": [
    {
      "name": "my-factorio-server",
      "state": "online",
      "address": "0.0.0.0:34197",
      "backend": "docker container factorio",
      "since": "2026-08-29T14:02:11Z",
      "players": {"count": 2, "names": ["Alice", "Bob"]},
      "idle_seconds": 0,
      "shutdown_in_seconds": 600,
      "inactivity_timeout_seconds": 600,
      "auto_shutdown": true,
      "listener_bound": false,
      "listing": {"enabled": true, "listed": false}
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `state` | `offline`, `starting`, `online`, `stopping` or `error` |
| `players` | Present only while online and RCON is answering |
| `idle_seconds` | How long the server has had zero players |
| `shutdown_in_seconds` | Remaining inactivity countdown |
| `auto_shutdown` | False when RCON is not configured, so napio cannot see players |
| `listener_bound` | Whether napio currently holds the UDP port |
| `listing` | The public listing, when matchmaking is enabled. Never contains credentials |
| `last_error` | Most recent failure, if any |
| `retry_in_seconds` | Remaining backoff while in `error` |

## `GET /api/v1/servers/{name}`

The same object for one server. `404` if there is no such server.

## `POST /api/v1/servers/{name}/start`

Starts the server. Returns `202` with the current status, `409` if the server
is mid-transition, `403` if `api.control` is false.

An explicit start clears any backoff from earlier failures.

## `POST /api/v1/servers/{name}/stop`

Graceful shutdown: `/save`, then `/quit`, then SIGTERM if needed. Same
response codes.

### The `listing` object

```json
{"enabled": true, "listed": true, "game_id": 1310857,
 "last_heartbeat": "2026-08-29T17:18:05Z"}
```

`listed` is true only while napio is advertising a sleeping server; a running
server publishes its own listing, so napio withdraws. `last_error` appears when
the matching service is unhappy — a rejected token, for example. The
factorio.com token, the listing secret and the RCON password are never included
in any response.

## Authentication

When `api.token` is set, every endpoint except `/healthz` requires:

```
Authorization: Bearer <token>
```

Comparison is constant-time. There is no login, no session and no cookie: this
is a machine interface. If you expose it beyond loopback, put it behind a
reverse proxy with TLS.
