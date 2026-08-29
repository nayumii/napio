#!/bin/sh
# Runs a real Factorio dedicated server against a lab matching server and
# captures everything it sends. See README.md.
set -eu
cd "$(dirname "$0")"

IMAGE="${FACTORIO_IMAGE:-factoriotools/factorio:stable}"
NET=napio-lab
MM=napio-lab-mm
FACTORIO=napio-lab-factorio

[ -f certs/ca.crt ] || ./gen-certs.sh

# The Factorio image validates TLS against its bundled bundle and the system
# store, so the lab CA is appended to both.
cid=$(docker create "$IMAGE")
docker cp "$cid:/opt/factorio/data/cacert.pem" certs/orig-cacert.pem >/dev/null
docker cp "$cid:/etc/ssl/certs/ca-certificates.crt" certs/orig-system-ca.crt >/dev/null
docker cp "$cid:/opt/factorio/data/server-settings.example.json" ./server-settings.example.json >/dev/null
docker rm "$cid" >/dev/null
cat certs/orig-cacert.pem certs/ca.crt > certs/cacert-lab.pem
cat certs/orig-system-ca.crt certs/ca.crt > certs/system-ca-lab.crt

mkdir -p log factorio/config
chmod 777 log
if [ ! -f factorio/config/server-settings.json ]; then
  python3 - <<'PY'
import json
d = json.load(open("server-settings.example.json"))
d.update({
    "name": "napio lab server",
    "description": "protocol capture lab",
    "tags": ["napio", "lab"],
    "visibility": {"public": True, "lan": True},
    # A public game needs a username and token to be present; the lab matching
    # server never checks them, so these are deliberately not real.
    "username": "napio-lab-user",
    "token": "0123456789abcdef0123456789abcd",
    "require_user_verification": True,
    "max_players": 8,
})
json.dump(d, open("factorio/config/server-settings.json", "w"), indent=2)
PY
  echo labrcon > factorio/config/rconpw
fi

docker network create "$NET" >/dev/null 2>&1 || true
docker rm -f "$MM" "$FACTORIO" >/dev/null 2>&1 || true

docker run -d --name "$MM" --network "$NET" \
  -v "$PWD:/app:ro" -v "$PWD/certs:/certs:ro" -v "$PWD/log:/log" \
  python:3.12-slim python /app/fakemm.py >/dev/null
sleep 2
MMIP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$MM")
echo "lab matching server at $MMIP"

docker run -d --name "$FACTORIO" --network "$NET" \
  --add-host "multiplayer.factorio.com:$MMIP" \
  --add-host "auth.factorio.com:$MMIP" \
  -p 34197:34197/udp -p 127.0.0.1:27015:27015/tcp \
  -v "$PWD/factorio:/factorio" \
  -v "$PWD/certs/cacert-lab.pem:/opt/factorio/data/cacert.pem:ro" \
  -v "$PWD/certs/system-ca-lab.crt:/etc/ssl/certs/ca-certificates.crt:ro" \
  -e PUID="$(id -u)" -e PGID="$(id -g)" \
  "$IMAGE" >/dev/null

cat <<'MSG'
Started. The first run generates a map, so give it a minute.

  docker logs -f napio-lab-factorio      # look for "Matching server game ... created"
  docker logs napio-lab-mm               # every captured request, one JSON per line

Shut down cleanly to capture remove-game:

  docker stop -t 30 napio-lab-factorio

Tear the lab down:

  docker rm -f napio-lab-factorio napio-lab-mm && docker network rm napio-lab
MSG
