# Matchmaking lab

Tools for investigating how a Factorio dedicated server registers itself with
`multiplayer.factorio.com`. The findings are written up in
[`docs/MATCHMAKING.md`](../../docs/MATCHMAKING.md).

Nothing here is part of the napio binary. It is a research rig, kept in the
repository so the findings can be re-checked against future Factorio versions.

## `run.sh` — capture what a real server sends

Runs a real Factorio dedicated server with `multiplayer.factorio.com` pointed at
a local HTTPS server that logs every request. `auth.factorio.com` is proxied to
the real service, because a public game will not start without a genuine
padlock, and that endpoint needs no credentials.

```console
$ ./run.sh
$ docker logs -f napio-lab-factorio     # wait for "Matching server game ... created"
$ docker logs napio-lab-mm              # one JSON object per captured request
$ docker stop -t 30 napio-lab-factorio  # captures remove-game
$ docker rm -f napio-lab-factorio napio-lab-mm && docker network rm napio-lab
```

Requires Docker and `openssl`. The lab CA it generates is throwaway and is used
only inside the lab container; `certs/`, `log/` and `factorio/` are ignored by
git.

The credentials in the generated `server-settings.json` are deliberately fake:
the lab matching server never checks them.

## `publish.py` — validate against the real service

This one talks to the **real** Factorio matching service with **your** account
token, to answer the open questions in `docs/MATCHMAKING.md`: whether a listing
can be published by something that is not a running Factorio server, and whether
a real client can find and join it.

```console
$ export FACTORIO_TOKEN=...        # from your factorio.com profile page
$ ./publish.py --settings /srv/factorio/config/server-settings.json \
               --host-address 203.0.113.7:34197 \
               --game-version 2.0.77 --build-version 84539 \
               --minutes 3
```

It always removes the listing again, including on Ctrl-C. **Keep the window
short:** a listing that points at a server nobody can join pollutes the public
browser for real players.

The token is read from the environment and is never printed, logged or written
to a file.
