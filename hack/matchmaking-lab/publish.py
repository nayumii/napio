#!/usr/bin/env python3
"""Publish one listing to the REAL Factorio matching service, for validation.

This is the experiment described in docs/MATCHMAKING.md: it answers whether
multiplayer.factorio.com accepts a listing published by something other than a
running Factorio server, and whether a real client can then find and join it.

It is a validation tool, not a product. It deliberately:

  * takes the token from the environment and never prints or logs it;
  * takes every advertised field from a real server-settings.json, so nothing
    is invented;
  * removes the listing again on exit, always. A listing pointing at a server
    nobody can join pollutes the public browser for real players.

Usage:

    export FACTORIO_TOKEN=...                     # from your factorio.com profile
    ./publish.py --settings /srv/factorio/config/server-settings.json \
                 --host-address 203.0.113.7:34197 \
                 --game-version 2.0.77 --build-version 84539 \
                 --minutes 3
"""
import argparse
import json
import os
import random
import signal
import string
import sys
import time
import urllib.error
import urllib.request

BASE = "https://multiplayer.factorio.com"
NO_GAME_ID = 4294967295  # 0xFFFFFFFF: "no id yet", as sent by the real server
HEARTBEAT_SECONDS = 30   # measured against a real server


def post(path, payload):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(BASE + path, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", "napio-matchmaking-lab/validation")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            body = resp.read().decode("utf-8", "replace")
            return resp.status, body
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")


def secret():
    alphabet = string.ascii_letters + string.digits
    return "".join(random.choice(alphabet) for _ in range(32))


def build_game(args, settings):
    """Every advertised field comes from the real server settings or the
    command line. Nothing here is guessed."""
    game = {
        "game_id": NO_GAME_ID,
        "game_secret": secret(),
        "name": settings["name"],
        "description": settings.get("description", ""),
        "tags": settings.get("tags", []),
        "application_version": {
            "game_version": args.game_version,
            "build_version": args.build_version,
            "build_mode": "headless",
            "platform": args.platform,
        },
        "max_players": settings.get("max_players", 0),
        "has_password": bool(settings.get("game_password")),
        "host_address": args.host_address,
        "host_port": int(args.host_address.rsplit(":", 1)[1]),
        "headless_server": True,
    }
    if args.mods:
        game["mods"] = json.loads(args.mods)
    if args.game_time_elapsed is not None:
        game["game_time_elapsed"] = args.game_time_elapsed
    return game


def main():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--settings", required=True, help="path to a real server-settings.json")
    p.add_argument("--host-address", required=True, help="public ip:port players would connect to")
    p.add_argument("--game-version", required=True, help='e.g. "2.0.77"')
    p.add_argument("--build-version", required=True, type=int, help="e.g. 84539")
    p.add_argument("--platform", default="linux64")
    p.add_argument("--mods", help='JSON array, e.g. \'[{"name":"base","version":"2.0.77"}]\'')
    p.add_argument("--game-time-elapsed", type=int, default=None,
                   help="ticks; omit unless you actually know it")
    p.add_argument("--minutes", type=float, default=3.0, help="how long to keep the listing up")
    args = p.parse_args()

    token = os.environ.get("FACTORIO_TOKEN")
    username = os.environ.get("FACTORIO_USERNAME")
    if not token:
        sys.exit("FACTORIO_TOKEN is not set (get it from your factorio.com profile page)")

    with open(args.settings) as fh:
        settings = json.load(fh)
    username = username or settings.get("username")
    if not username:
        sys.exit("no username: set FACTORIO_USERNAME or put one in server-settings.json")

    game = build_game(args, settings)
    game_secret = game["game_secret"]

    print("publishing listing for %s at %s ..." % (game["name"], game["host_address"]))
    status, body = post("/create-game", {"game": game, "username": username, "token": token})
    print("create-game -> HTTP %d: %s" % (status, body[:400]))
    if status != 200:
        sys.exit("create-game failed; nothing to clean up")

    try:
        game_id = json.loads(body)["game_id"]
    except Exception:
        sys.exit("could not read game_id from the response; nothing to clean up")

    print("game_id=%s" % game_id)
    print("check it: curl -s %s/get-game-details/%s | jq" % (BASE, game_id))
    print("now open a real Factorio client and look for it in Browse public games")

    stop = {"now": False}
    signal.signal(signal.SIGINT, lambda *_: stop.__setitem__("now", True))

    deadline = time.time() + args.minutes * 60
    try:
        while not stop["now"] and time.time() < deadline:
            time.sleep(min(HEARTBEAT_SECONDS, max(0, deadline - time.time())))
            if stop["now"]:
                break
            status, body = post("/post-game-heartbeat/%s" % game_id,
                                {"game_secret": game_secret, "game_patch": {}})
            print("heartbeat -> HTTP %d: %s" % (status, body[:200]))
    finally:
        status, body = post("/remove-game/%s" % game_id, {"game_secret": game_secret})
        print("remove-game -> HTTP %d: %s" % (status, body[:200]))


if __name__ == "__main__":
    main()
