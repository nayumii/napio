# Changelog

All notable changes to this project are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] — 2026-08-29

First release. Small on purpose.

### Added

- Wake-on-connect for Factorio dedicated servers, using port handover rather
  than a proxy: napio holds the game's UDP port only while the server is
  asleep, and is completely out of the network path while it runs.
- Behavioural wake filter (repeated datagrams from one source) that
  distinguishes a retrying Factorio client from a port scanner without
  parsing any game protocol.
- Readiness detection via RCON, which the Factorio server only starts once the
  save has loaded.
- Player detection via `/players online`, using plain console commands so
  achievements are never disabled.
- Graceful shutdown ladder: `/save`, `/quit`, then SIGTERM, then SIGKILL.
- Configurable inactivity timeout per server, paused rather than advanced when
  the player count is unknown.
- Multiple independent servers, each with its own port, backend, timeout and
  status.
- Docker backend (three daemon endpoints, no Docker SDK dependency) and a
  process backend.
- Crash, startup-failure and shutdown-failure handling with exponential
  backoff, and adoption of servers that are already running.
- Optional HTTP API for status and manual start/stop, designed as the surface
  a future web UI can be built on.
- Documentation: architecture and validation, configuration, deployment, API,
  troubleshooting, security.
