# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-09-25

First release. Covers the MVP, Phase 2 and Phase 3 from [docs/PLAN.md](docs/PLAN.md).

### Added

- **Identity:** worktrees are discovered from `git worktree list`. The worktree ID is the git admin dir, so it survives `git worktree move` and branch renames. Hostname labels are sanitised, handle collisions, and can be pinned with `--name` / `WG_WORKTREE`.
- **Local routing:** `{worktree}.{project}.localhost` hostnames, served by embedded Caddy or by an existing Caddy through its admin API. HTTPS uses Caddy's internal CA, trusted only when you run `wtg trust`.
- **Daemon:** auto-started, controlled over a 0600 unix socket. It runs health checks (PID plus TCP or HTTP) and a discovery loop that follows worktree add, remove, rename and move. `wtg down --forget` deregisters safely.
- **Getting started:** `wtg init` writes a starter `wtg.yaml` and guesses the dev command. `wtg open` opens the worktree URL. An `install.sh` script installs release binaries.
- **Commands:** `wtg up`, `down`, `run`, `register`, `deregister`, `status`, `port`, `env`, `doctor`, `hosts`, `trust`, `untrust`.
- **Services:** multiple services per worktree with hostname templates, stable port allocation (20000–29999, clear of Worktrunk's `hash_port` range), and `WG_*` identity env injection.
- **Worktrunk:** hooks via `wtg hooks worktrunk --write`.
- **Public ingress:** opt-in, with a per-service path allowlist, path normalisation, and requests held while a service restarts. Tunnels: cloudflared (quick or named), ngrok, or external.
- **Request log:** bounded and redacted, with source detection (Stripe, GitHub, Slack and others), plus guarded replay with `wtg replay`.
- **OAuth callbacks:** routed to the right worktree using HMAC-signed, expiring, single-use state. Supports GET and `form_post`.

[Unreleased]: https://github.com/chryzxc/worktree-gateway/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/chryzxc/worktree-gateway/releases/tag/v0.1.0
