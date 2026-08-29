# Troubleshooting

Start with `log_level: debug` and `napio -config napio.yaml -check`.

## The server never wakes up

**napio is not holding the port.** Check `listener_bound` in the API, or the
log line `listening for wake-up traffic`. If the bind failed, something else
owns the port — usually the Factorio container itself, still running or
configured with `restart: always`.

```console
$ ss -lunp | grep 34197
$ docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' factorio   # must be "no"
```

**The traffic never arrives.** Confirm the port is forwarded to the machine
running napio and that you are connecting to the right address:

```console
$ sudo tcpdump -ni any udp port 34197
```

**The wake filter dropped it.** By default napio needs two datagrams from the
same address within five seconds. A single crafted test packet will not do it;
a real Factorio client will. To test by hand:

```console
$ for i in 1 2 3; do echo "wake up please" | nc -u -w1 SERVER 34197; done
```

## The player still can't connect after the server wakes

That is expected on the first attempt. The Factorio client gives up after
about ten seconds; a cold start usually takes longer. Reconnecting after
half a minute joins normally. Watch it happen:

```console
$ curl -s localhost:8378/api/v1/servers | jq '.servers[].state'
```

## `docker start` fails with "driver failed programming external connectivity"

Something still holds the published port. napio releases its socket before
starting a container, so this normally means a second napio instance is
running, or a previous Factorio container is still up:

```console
$ ss -lunp | grep 34197
$ docker ps -a | grep factorio
```

## The server starts but napio says it never became ready

napio's readiness signal is RCON answering, because Factorio only starts its
RCON interface once the save is loaded. If RCON never comes up:

- Is RCON enabled and published? `docker port <container>` should show the
  RCON port. With `factoriotools/factorio` the password lives in
  `config/rconpw` inside the volume.
- Is the address in `napio.yaml` reachable *from napio*? Inside a container
  with `network_mode: host`, `127.0.0.1:27015` is the host's loopback.
- Did Factorio actually finish loading? `docker logs <container>` — a corrupt
  save, a mod mismatch or a missing `token` for a public game all stop it
  before RCON starts.

Common startup failures seen in real logs:

| Log line | Cause |
|---|---|
| `Matching server connection failed: ... Missing token` | `visibility.public: true` without a factorio.com token in `server-settings.json` |
| `require_user_verification must be enabled for public games` | Public game with `require_user_verification: false` |
| `Failed to reach auth server` | The server has no DNS or no outbound internet. Factorio needs to reach `auth.factorio.com` at startup for a public game |
| `Map version ... newer than the game version` | Save made by a newer Factorio than the image |

napio treats all of these the same way: the start fails, it backs off (30 s,
doubling to 5 minutes), takes the port back so players can trigger a retry,
and reports the failure in `last_error`.

## The server shuts down while people are playing

It should be impossible: napio only counts down when a poll reports zero
players, cancels the countdown the moment it sees anyone, and *pauses* rather
than advances the countdown when RCON is unreachable. If you see it happen,
please open an issue with `log_level: debug` output — that is a bug worth
fixing properly.

Note the countdown is checked once per `poll_interval`, so shutdown can happen
up to one poll after the timeout expires. That is not a bug.

## The server never shuts down

- Is RCON configured? Without it `auto_shutdown` is `false` and napio will not
  guess. The startup log warns about this.
- Is somebody actually connected? `curl -s localhost:8378/api/v1/servers`.
- Is the RCON poll failing? Look for `player probe failed; inactivity
  countdown paused`.

## napio was restarted and lost track of a server

napio keeps no state on disk; it re-reads reality at startup. A running
container is adopted as `online`, a stopped one as `offline`. The **process**
backend cannot do this — a child process does not survive its parent — which
is why Docker is the recommended backend.

## Everything looks right but the server is not in the public list

While asleep, Factorio's own listing is gone: it is a heartbeat published by the
running process, every 30 seconds, withdrawn when that process exits. napio can
publish one on its behalf, but only if you enabled `matchmaking` — see
[MATCHMAKING.md](MATCHMAKING.md).

While awake, the listing has nothing to do with napio — check
`server-settings.json` for `visibility.public`, `username` and `token`, and look
for these lines in the Factorio log:

```
Info AuthServerConnector.cpp:112: Obtained serverPadlock for serverHash (...)
Info ServerRouter.cpp:547: Own address is IP ADDR:({...:34197}) (confirmed by pingpong1)
Info MatchingServer.cpp:129: Matching server game `NNNN` has been created.
```

If the last one is missing, the server never registered. `Missing token` means
no factorio.com token in `server-settings.json`; a failure at the padlock step
means the server could not reach `auth.factorio.com`.

## The sleeping server is not listed, with matchmaking enabled

Check `listing` in the API:

```console
$ curl -s localhost:8378/api/v1/servers | jq '.servers[].listing'
```

| What you see | Meaning |
|---|---|
| `"enabled": false` or no `listing` at all | `matchmaking.enabled` is not set for this server |
| `"listed": false` with `last_error` mentioning credentials | The factorio.com token was rejected (`HTTP 401 Token doesn't match.`). Get a fresh one from your profile page |
| `"listed": false` with `last_error` about the version banner | napio has never seen this server start, so it does not know which build to advertise and refuses to guess. Start the server once |
| `"listed": false` with `last_error` about the mod list | `mods/mod-list.json` could not be read. napio will not advertise an unknown mod set |
| `"listed": false` while the server is `online`, `starting` or `error` | Correct: napio only advertises a server that is asleep and wakeable |
| `"listed": true` but nothing in the browser | The listing exists — check `curl https://multiplayer.factorio.com/get-game-details/<game_id>`. Public browsing needs the client to be logged in |

A player who joins from the browser and gets nothing usually means the
advertised address is not actually reachable. Confirm the port is forwarded to
the machine running napio, and watch with `log_level: debug`, which logs every
datagram that arrives:

```
level=DEBUG msg="datagram received" from=203.0.113.9 bytes=57 wakes_server=false
level=INFO  msg="connection attempt detected" from=203.0.113.9
```

If nothing is logged at all, the traffic never arrived. If datagrams arrive but
`wakes_server` stays false, the wake filter needs a second packet — a real
client retries, so this resolves itself within a second or two.

### Listed, but nobody can join while it sleeps

If the *running* server is reachable and the *sleeping* one is not, the cause is
almost always one of these two. Both were hit during the original verification.

**A host firewall is dropping the traffic.** Docker publishes ports by inserting
its own DNAT and FORWARD rules, which bypass `ufw` entirely. A sleeping napio is
an ordinary host process on the `INPUT` chain, so it is not bypassed:

```console
$ sudo ufw status | grep 34197      # nothing? that is the problem
$ sudo ufw allow 34197/udp
```

That asymmetry produces exactly the confusing symptom above: the server works
when awake and is unreachable when asleep, with napio logging no datagrams at
all.

**NAT hairpin.** A client on the same LAN as the server usually cannot reach the
router's WAN address; most consumer routers do not loop UDP back. The public
browser hands the client your WAN address, so a same-LAN test fails no matter
what napio does — and fails identically against the running server, which is the
quickest way to tell the two apart. Test from outside the network.

If a Join from the public browser produces no datagrams at all, while a direct
connect to the same address from the same machine does, that is a genuine
finding — please report it at <https://git.naknu.li/emily/napio/issues>.
