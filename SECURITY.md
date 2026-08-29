# Security

## Reporting a vulnerability

Please report security issues privately by opening a confidential issue on the
project's Forgejo instance, or by contacting the maintainer directly, rather
than in a public issue. You can expect an acknowledgement within a few days.

## Threat model

napio sits between the public internet and a game server, so it is worth being
precise about what it does and does not do.

**napio never parses game traffic.** While a server is asleep, napio reads
datagrams from the UDP port and looks at exactly two things: the source
address and the length. Nothing derived from packet contents is ever parsed,
allocated from, or forwarded. While a server is awake, napio does not touch
the port at all.

**Waking a server is an unauthenticated action.** Anyone who can reach the UDP
port can cause a start. That is the entire point of the tool, but it can be
abused to make a machine do work repeatedly. Mitigations:

- The wake filter requires repeated datagrams from one source, so single-packet
  scans are ignored.
- `wake.cooldown` rate-limits wake-ups.
- `wake.allow_from` restricts wake-ups to given CIDRs.
- `wake.disabled` turns wake-on-connect off entirely, leaving API-only control.
- Failed starts back off exponentially, so a broken server cannot be hammered.

Joining the server still requires whatever authentication Factorio itself
enforces; napio grants no access.

**RCON is a privileged interface.** Its password lets anyone run server console
commands. Publish it on loopback only (`127.0.0.1:27015:27015/tcp`), give it a
long random password, and keep the password in an environment variable rather
than in the config file — napio expands `${VAR}` in `rcon.password` for exactly
this reason.

**The Docker socket is root-equivalent.** A napio instance with access to
`/var/run/docker.sock` can do anything Docker can. If that is not acceptable,
put a socket proxy in front of it that permits only
`GET /containers/{id}/json`, `POST /containers/{id}/start` and
`POST /containers/{id}/stop`, and point `backend.host` at the proxy — those are
the only three endpoints napio uses.

**The HTTP API** binds `127.0.0.1` by default, is read-only unless
`api.control` is enabled, and supports a bearer token compared in constant
time. It never returns RCON passwords or API tokens. If you expose it beyond
loopback, put it behind a reverse proxy with TLS.

**napio makes no outbound connections** other than to your Docker daemon and
your Factorio server's RCON port. It does not talk to factorio.com and never
handles your factorio.com credentials.

## Dependencies

napio depends on the Go standard library plus `gopkg.in/yaml.v3`. That is a
deliberate constraint: a supervisor that runs unattended on someone's home
server should be small enough to read.
