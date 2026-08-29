# napio

**Wake-on-connect for Factorio dedicated servers.** Your NAS stays on; your
Factorio server doesn't. napio watches the game port while a server is asleep,
starts it when somebody tries to connect, and shuts it down again once it has
been empty for a while.

```
player connects ──▶ napio hears the knock ──▶ starts Factorio ──▶ gets out of the way
                                                                        │
        0 players for 10 minutes ◀── /save, /quit ◀── napio watches ◀────┘
```

- **No mods, no custom client.** Players use the normal Factorio client.
- **Not a proxy.** While the server runs, napio is not in the network path at
  all — so the public server list, NAT punching and latency are exactly what
  they would be without napio.
- **Small.** A single static binary, no database, a few MB of RAM. Perfectly
  happy on a Raspberry Pi.
- **Multiple servers**, each with its own port, container and timeout.

## Why not just a proxy?

Because Factorio isn't Minecraft. The game protocol is UDP and undocumented,
and the Factorio server registers itself with the matching server, discovers
its own public address and performs NAT punching **from its own socket**. A
relay in front of that changes the address players are told to expect, and
risks breaking both public-list visibility and NAT punching for a
[claimed ~70 %](https://factorio.com/blog/post/fff-143) of players.

So napio does something simpler and safer: it holds the UDP port **only while
the server is asleep** and hands it straight back when the server starts. See
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full reasoning, the
measurements it is based on, and the alternatives that were rejected.

## Quick start (Docker)

You already have a Factorio container. Tell napio about it:

```yaml
# napio.yaml
servers:
  - name: my-factorio-server
    address: 0.0.0.0
    port: 34197
    inactivity_timeout: 10m
    rcon:
      address: "127.0.0.1:27015"
      password: "${FACTORIO_RCON_PASSWORD}"
    backend:
      type: docker
      container: factorio
```

Your Factorio container needs three things:

```yaml
services:
  factorio:
    image: factoriotools/factorio:stable
    restart: "no"                          # napio decides when it runs
    ports:
      - "34197:34197/udp"                  # the port napio guards
      - "127.0.0.1:27015:27015/tcp"        # rcon, loopback only
```

Then:

```console
$ napio -config napio.yaml -check     # validate first
$ napio -config napio.yaml
```

Full walkthrough: [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

## What a player sees

| Situation | Experience |
|---|---|
| Server awake | A completely ordinary Factorio server. Public list, direct connect, everything. |
| Server asleep, player connects | The first attempt wakes the server and usually times out after ~10 s (that is all the patience the Factorio client has). The second attempt, half a minute later, joins. |
| Server asleep | It is not in the public server browser, because that listing is published by the running Factorio process. Players wake it with a direct connect. ([Could napio publish it? The protocol is documented; the blocker is validation.](docs/MATCHMAKING.md)) |

Neither of those is a bug we can fix from outside the game; both are explained
in [Limitations](docs/ARCHITECTURE.md#10-limitations-imposed-by-factorio-itself).

## Status API

```console
$ curl -s localhost:8378/api/v1/servers | jq
{
  "servers": [
    {
      "name": "my-factorio-server",
      "state": "online",
      "address": "0.0.0.0:34197",
      "players": {"count": 2, "names": ["Alice", "Bob"]},
      "idle_seconds": 0,
      "listener_bound": false
    }
  ]
}
```

Enough for a web UI to be built on later, without napio having to grow one.
See [docs/API.md](docs/API.md).

## Documentation

| | |
|---|---|
| [Architecture](docs/ARCHITECTURE.md) | How it works, why it works, what was measured, what was rejected |
| [Matchmaking](docs/MATCHMAKING.md) | The captured public-listing protocol, and why napio does not use it yet |
| [Configuration](docs/CONFIGURATION.md) | Every option |
| [Deployment](docs/DEPLOYMENT.md) | Docker, compose, systemd, multiple servers |
| [API](docs/API.md) | HTTP endpoints |
| [Troubleshooting](docs/TROUBLESHOOTING.md) | When it doesn't work |
| [Security](SECURITY.md) | Threat model and reporting |
| [Contributing](CONTRIBUTING.md) | Development setup |

## Requirements

- Linux, macOS or Windows (Linux is what it is tested on)
- Factorio 2.0 headless server, with RCON enabled
- Docker, or a Factorio server napio can start as a child process
- napio must be able to bind the game's UDP port, so it runs on the host or in
  a container with `network_mode: host`

## License

MIT. See [LICENSE](LICENSE).
