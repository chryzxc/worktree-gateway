# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.1] - 2026-09-25

### Fixed

- **Daemon auto-start race:** when several commands started the daemon at once (for example Worktrunk hooks in two worktrees), the losing daemon could delete the winner's socket, leaving an unreachable daemon that held the ports. The daemon now holds a lock file for its lifetime, and a CLI that loses the race uses the daemon that won.
- **Health checks acting on stale data:** a slow probe could mark a just re-registered service as down, or deregister it. Updates now apply only if the registration is unchanged.
- **Health checks run concurrently**, so one slow `health` endpoint no longer delays the others. Probes no longer follow redirects.
- **`wtg status` ordering** used an inconsistent sort comparator.
- **`go install` builds** now report their module version instead of `0.1.0-dev`.

### Added

- `wtg daemon restart`, and a warning when the running daemon's version differs from the CLI's (e.g. after an upgrade).
- Shell completion docs, plus upgrade and uninstall instructions.

### Changed

- The repository is public. `install.sh` downloads release assets directly with curl or wget, so it no longer needs the GitHub CLI or a token.

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

[Unreleased]: https://github.com/chryzxc/worktree-gateway/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/chryzxc/worktree-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/chryzxc/worktree-gateway/releases/tag/v0.1.0
