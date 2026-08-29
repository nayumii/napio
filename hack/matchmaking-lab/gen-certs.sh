#!/bin/sh
# Generates a throwaway CA and a certificate covering the Factorio service
# hostnames, so a dedicated server can be pointed at the lab matching server.
set -eu
cd "$(dirname "$0")"
mkdir -p certs
cd certs

openssl req -x509 -newkey rsa:2048 -sha256 -days 30 -nodes \
  -keyout ca.key -out ca.crt -subj "/CN=napio-lab-ca" \
  -addext "basicConstraints=critical,CA:TRUE"

openssl req -newkey rsa:2048 -nodes -keyout srv.key -out srv.csr \
  -subj "/CN=multiplayer.factorio.com"

cat > san.cnf <<'SAN'
subjectAltName=DNS:multiplayer.factorio.com,DNS:auth.factorio.com,DNS:factorio.com,DNS:www.factorio.com
extendedKeyUsage=serverAuth
SAN

openssl x509 -req -in srv.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out srv.crt -days 30 -sha256 -extfile san.cnf

echo "lab CA and certificate written to $(pwd)"
