# Deployment

## The one rule

napio and the Factorio server must be able to bind **the same host UDP port**,
one at a time. napio holds it while the server sleeps and releases it before
the server starts.

That means:

- napio runs **on the host**, or in a container with `network_mode: host`.
- The Factorio container publishes the game port on the host
  (`-p 34197:34197/udp`).
- The Factorio container must **not** restart itself
  (`restart: "no"`), or it will fight napio for control. napio warns about
  this at startup.

## Docker Compose

`examples/docker-compose.yml` is a complete working example:

```yaml
services:
  factorio:
    image: factoriotools/factorio:stable
    container_name: factorio
    restart: "no"
    ports:
      - "34197:34197/udp"
      - "127.0.0.1:27015:27015/tcp"
    volumes:
      - ./factorio:/factorio
    environment:
      - RCON_PASSWORD=${FACTORIO_RCON_PASSWORD}

  napio:
    image: napio:latest
    container_name: napio
    restart: unless-stopped
    network_mode: host          # napio must own the game port on the host
    volumes:
      - ./napio.yaml:/etc/napio/napio.yaml:ro
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      - FACTORIO_RCON_PASSWORD=${FACTORIO_RCON_PASSWORD}
    command: ["-config", "/etc/napio/napio.yaml"]
```

Start Factorio once by hand so the save exists and RCON works, stop it, then
start napio. From then on napio owns the lifecycle:

```console
$ docker compose up -d factorio
$ docker compose logs -f factorio        # wait for "Starting RCON interface"
$ docker compose stop factorio
$ docker compose up -d napio
```

napio adopts whatever it finds: if the container is already running when napio
starts, it moves straight to `online` without touching it.

## Running napio on the host

```console
$ go build -o napio ./cmd/napio           # or download a release binary
$ sudo install -m 0755 napio /usr/local/bin/napio
$ sudo install -d /etc/napio
$ sudo install -m 0640 examples/napio.yaml /etc/napio/napio.yaml
```

`examples/napio.service` is a systemd unit:

```ini
[Unit]
Description=napio — wake-on-connect gateway for Factorio
After=network-online.target docker.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/napio -config /etc/napio/napio.yaml
EnvironmentFile=/etc/napio/napio.env
Restart=on-failure
RestartSec=5
DynamicUser=yes
SupplementaryGroups=docker
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

```console
$ sudo systemctl enable --now napio
$ journalctl -u napio -f
```

## Pelican / Pterodactyl

Both panels run the same Wings daemon, and napio talks to Wings rather than to
the panel. The setup is the same for either; `type: pterodactyl` is accepted as
a spelling of `type: pelican`.

### Before you start

**napio has to run on the Wings node**, on the host or in a container with
`network_mode: host`. The one rule at the top of this page has not changed:
napio holds the game's UDP port while the server sleeps, so it must be on the
machine that publishes that port. Pointing napio at a Wings node somewhere else
will start and stop the server correctly and wake it never — the port it guards
is not the port players reach.

### 1. Find the three values you need

**The node token.** On the node, in the Wings configuration:

```console
$ sudo grep -A2 '^token' /etc/pelican/config.yml     # /etc/pterodactyl/ on Pterodactyl
```

Use the long `token:` value, not `token_id:`. It is the node's master
credential — put it in an environment variable, never in `napio.yaml`.

**The server UUID.** The panel shows it on the server's settings page. It is the
full UUID (`d3aac109-e5a0-4331-b03e-c8cec1cbfb0d`), *not* the eight-character id
in the panel URL — Wings does not answer to the short one. On the node it is
also the directory name under `/var/lib/pelican/volumes/`.

**The allocation.** The server's default allocation in the panel is the port
napio must guard. napio compares the two at startup and warns if they disagree.

### 2. Enable RCON on the server

In the panel, on the server's startup page, set `RCON Enabled` to `1`, give
`RCON Password` a value, and set `RCON Port` to a second allocation. napio uses
RCON to know when the server has finished loading, to count players, and to save
the map before shutting down.

The stock Factorio egg stops the server with `/quit`, which does **not** save.
Without RCON, every shutdown loses everything since the last autosave.

### 3. Configure napio

```yaml
servers:
  - name: factorio
    address: 0.0.0.0
    port: 34197                     # the server's default allocation
    inactivity_timeout: 10m
    rcon:
      address: "127.0.0.1:27015"    # the RCON allocation
      password: "${FACTORIO_RCON_PASSWORD}"
    backend:
      type: pelican
      wings_url: "https://node.example.com:8080"
      uuid: "d3aac109-e5a0-4331-b03e-c8cec1cbfb0d"
      token: "${PELICAN_WINGS_TOKEN}"
```

`wings_url` is the daemon's own address. Use the node's FQDN rather than
`127.0.0.1`: Wings normally serves a certificate for that name, and a request
to `127.0.0.1` fails to verify against it. For a node with a self-signed
certificate, set `tls_skip_verify: true`.

Then:

```console
$ napio -config napio.yaml -check
$ PELICAN_WINGS_TOKEN=... FACTORIO_RCON_PASSWORD=... napio -config napio.yaml
```

napio checks the allocation against the port it guards on startup and logs a
warning if they do not match, so watch the first few lines of the log.

### Keeping the sleeping server in the public list

This works the same as anywhere else (see below), but the paths differ: the
Pelican egg keeps Factorio's settings at `data/server-settings.json`, not
`config/`, so point napio at the files directly rather than using `data_dir`:

```yaml
    matchmaking:
      enabled: true
      public_address: "203.0.113.7:34197"
      server_settings: /var/lib/pelican/volumes/<uuid>/data/server-settings.json
      mods_dir: /var/lib/pelican/volumes/<uuid>/mods
```

napio reads those files from the node's disk, which is another reason it has to
run on the node. The napio process needs read access to them.

### What napio does differently here

Wings watches the server process and restarts anything that exits without its
say-so, so napio never sends `/quit` itself on this backend — it saves over
RCON and then asks Wings to stop the server, which sends the egg's own `/quit`.
The result is a clean, saved shutdown that Wings knows about and does not undo.
Leave crash detection alone; it does not need turning off.

## Ports below 1024

Factorio's default port is 34197, so this rarely comes up. If you do bind a
privileged port, grant the capability instead of running as root:

```console
$ sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/napio
```

## Multiple servers

Give each one its own UDP port, container and RCON port. Nothing else is
needed — napio runs an independent supervisor per server, and one server
failing does not affect the others.

```yaml
services:
  factorio-main:
    image: factoriotools/factorio:stable
    container_name: factorio-main
    restart: "no"
    ports:
      - "34197:34197/udp"
      - "127.0.0.1:27015:27015/tcp"
    volumes: [./main:/factorio]

  factorio-modded:
    image: factoriotools/factorio:stable
    container_name: factorio-modded
    restart: "no"
    ports:
      - "34198:34197/udp"
      - "127.0.0.1:27016:27015/tcp"
    volumes: [./modded:/factorio]
```

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

## Keeping the server in the public list

While the server is awake it registers itself with Factorio's matching server
exactly as it always would — napio is not involved and not in the way. Set
your `token` and `username` in `server-settings.json` and
`visibility.public: true` as usual.

While the server is asleep, Factorio's own listing is gone — it is a heartbeat
published by the running process. Two options:

**Do nothing.** Players wake a sleeping server with a direct connect to your
address; the client remembers the last address used.

**Let napio publish the listing while the server sleeps** (opt-in):

```yaml
    matchmaking:
      enabled: true
      data_dir: /srv/factorio                # holds config/ and mods/
      public_address: "203.0.113.7:34197"    # what players actually connect to
      token: "${FACTORIO_TOKEN}"
```

```console
$ FACTORIO_TOKEN=$(cat /etc/napio/factorio-token) napio -config napio.yaml
```

napio then advertises the sleeping server using the name, description, tags,
mod list and exact build read from the server's own files, withdraws that
listing the moment the server starts, and publishes it again when the server
goes back to sleep. Keep `visibility.public: true` in `server-settings.json` so
the awake server keeps listing itself.

Verified with a real client: the sleeping server was found in Browse public
games and joining it woke the server and let the player in, about thirteen
seconds from clicking Join.

Two things must be true for that to work, and neither is napio's doing:

- The advertised address must be reachable **by the napio process**. Docker's
  published ports bypass `ufw`; an ordinary host process does not. If the
  running server is reachable but the sleeping one is not, add
  `sudo ufw allow 34197/udp`.
- A client on the same LAN generally cannot reach your own WAN address (NAT
  hairpin). Test from outside the network.

**Read [MATCHMAKING.md](MATCHMAKING.md) before enabling it.** The protocol is
undocumented and can change with any Factorio release.

## Resource usage

napio is idle almost all of the time: one UDP socket per sleeping server, one
RCON poll every 10 s per running server. Expect single-digit MB of RSS and no
measurable CPU. It is meant to be left running forever on a NAS or a Pi.
