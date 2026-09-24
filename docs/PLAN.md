# Worktree Gateway — Implementation Plan

Status: executed through Phase 3 (see "Execution status" at the bottom).

## 1. Audit of existing tools (September 2026)

| Tool | What it does | Overlap | What it does not do |
|---|---|---|---|
| **Worktrunk** (`wt`) | Worktree create/switch/remove/merge, hooks (`pre-start`, `post-start`, `pre-remove`, `post-remove`, …), per-branch vars, `hash_port` filter (10000–19999) | None on routing — it is the recommended layer *underneath* us | No hostnames, no proxy, no callback routing |
| **Portless** (vercel-labs, TS, Apache-2.0) | Named `*.localhost` URLs, own HTTPS proxy + own CA, branch-as-subdomain in worktrees, `alias` for static routes, Tailscale/ngrok sharing, `prune` | **High** for "stable local URL per worktree" | Own proxy stack (not an established proxy); app-centric not worktree-centric (no worktree object grouping services); no reaction to worktree removal/rename; no path-based public routing to a worktree; no signed OAuth callback routing; no request log/replay |
| **Portree** (Go, MIT) | `.portree.toml`, multi-service per worktree, FNV port hashing, `*.localhost` proxy, env injection (`PT_BACKEND_URL`), process supervisor + TUI | **High** for multi-service local routing | Is a process supervisor (explicit non-goal for us); no external callbacks; no worktree-manager integration |
| **Lerd** | Herd-like PHP env, `.test` domains, nginx, worktree branch domains, TLS | Medium (PHP-centric) | Tied to its own runtime stack (Podman, PHP) |
| **Isola / Devflow / others** | Worktree environment isolation / agent dev flows | Low–medium | Environment isolation, not routing identity |
| **Caddy** | Production-grade proxy with JSON config, zero-downtime reloads, internal CA for local HTTPS, HTTP/2, WebSockets | We *use* it | — |
| **cloudflared / ngrok / Tailscale Funnel** | Public tunnels | We *use* them | — |

### Conclusion — the differentiator that remains

"A port per branch" and "a `*.localhost` name per branch" are solved (Portless, Portree).
What is **not** solved, and is therefore the product:

1. **Worktree as a first-class runtime object** — one identity (`project/worktree`) owning N services,
   with lifecycle tracked from Git itself (move/rename/remove), independent of who created the worktree.
2. **Registration-only core** — services announce themselves (`wtg register`, Worktrunk hook, or the thin
   `wtg run` convenience wrapper). We never supervise processes.
3. **External callback routing to a worktree** — path/host-routed public ingress with an allowlist,
   HMAC-signed OAuth state that routes a callback to the worktree that started the flow, and a bounded,
   redacted, worktree-aware request log with guarded replay.
4. **No new networking** — local routing is Caddy (embedded, or an external Caddy via its admin API);
   public reachability is cloudflared/ngrok/an existing tunnel.

Scope was deliberately *not* expanded to overlap with Portless's own proxy/CA work.

## 2. Answers to the open questions

| # | Question | Decision |
|---|---|---|
| 1 | Overlapping tools | See audit above. |
| 2 | Differentiator | Worktree identity + lifecycle + external callback routing (§1). |
| 3 | Wildcard hostname strategy | Default domain `{project}.localhost`. RFC 6761 `.localhost` resolves to loopback in Chrome/Firefox/Safari, curl ≥7.85 and systemd-resolved, **no privileged mutation**. Custom domains (`.test`) are allowed; `wtg hosts` *prints* `/etc/hosts` lines and `wtg doctor` checks resolution — we never edit system files. `.local` is discouraged (mDNS conflict on macOS). |
| 4 | Caddy / Traefik / library | **Caddy embedded as a Go library** (single binary, JSON config, `caddy.Load` hot reload, internal CA). A second provider pushes the identical JSON to an external Caddy admin API. Traefik rejected: no embeddable library story, file/label-driven config. |
| 5 | Registration vs runner | Registration is the core. `wtg run <svc>` is a thin wrapper: pick port → inject identity env → exec → register with PID → deregister on exit. No restarts, no log management, no multi-process orchestration. |
| 6 | Branch → hostname | `slug.Sanitize`: NFKD-ish ASCII fold, lowercase, `[^a-z0-9]`→`-`, collapse, trim, ≤63 chars (truncate + 4-hex hash of the original). Empty → `wt-<hash>`. Collisions inside a project get `-<hash4>` suffix (first registrant keeps the clean slug; assignment is persisted so it is stable). Detached HEAD uses the worktree directory name. Override with `--name` / `WG_WORKTREE`. |
| 7 | Stale detection | Health loop (2 s): PID liveness (if known) → remove; TCP dial fails → `down` (route answers 503 with an explanation) → removed after `stale_after` (10 min) for PID-less registrations. Git discovery loop (10 s): worktree gone/prunable → remove; branch renamed → re-slug; path moved → matched by the stable git admin-dir id. A new live registration on the same `host:port` evicts the previous owner (two processes cannot both listen). |
| 8 | Multi-service representation | `wtg.yaml` at the repo root, `services:` map with `port`, `command`, `hostname` template, `health`, `public`, `oauth_callbacks`. Registrations for services not in the file are still allowed (ad-hoc) with the default hostname template. |
| 9 | Request logging in core | Only for **public ingress** traffic. Bounded ring (500 entries, 7 days, 1 MiB body cap), credential headers redacted, body capture can be disabled. No local-traffic logging, no dashboards, no fan-out/transform (not Hookdeck). |
| 10 | First tunnel provider | **cloudflared** (quick tunnels need no account; named tunnels support stable hostnames). `ngrok` and `external` (bring-your-own tunnel, just declare the public URL) are also implemented behind the same interface. |
| 11 | Signing OAuth state | `base64url(json payload).base64url(HMAC-SHA256)` with a **per-worktree derived key** `HMAC(master, "wg-oauth-v1:"+project+"/"+worktree)`. The payload names project/worktree/service/path/inner-state/expiry/nonce. Tampering with the worktree changes the key → fails. Nonces are single-use. The derived key can be injected as `WG_OAUTH_STATE_KEY` so apps sign without IPC; `wtg oauth state` does it from the CLI. |
| 12 | Daemon needed? | Yes, one lightweight user-level daemon (`wtg daemon run`), auto-started by the CLI. It owns the proxy, the registry, health/discovery loops and the tunnel. Admin API is HTTP over a **unix socket** (0600) — never a TCP port, so no DNS-rebinding/CSRF surface. |

## 3. Runtime identity data model

```
Project   { id (hash of git common dir), name, slug, root (main worktree), common_dir, domain }
Worktree  { id = project.id + ":" + admin-dir-name ("." for main), project_id, slug, branch,
            path, head, is_main, pinned_name, created_at }
Service   { worktree_id, name, host (loopback only), port, pid, source (run|register|hook),
            hostnames[], status (starting|up|down), registered_at, last_up, public paths, oauth paths }
Route     derived, never stored: hostname → (worktree, service) → 127.0.0.1:port
```

Registry is persisted to `$STATE/registry.json` (atomic write). Everything derived (routes, Caddy
JSON, env vars) is recomputed from it.

## 4. Architecture

```
 CLI (wtg) ──unix socket JSON API──► daemon
                                      ├─ registry + slug allocator (persisted)
                                      ├─ discovery loop  (git worktree list --porcelain)
                                      ├─ health loop     (pid + TCP dial)
                                      ├─ proxy provider  ─► Caddy (embedded | external admin API)
                                      │                     :8780 http, :8743 https (internal CA)
                                      ├─ ingress (127.0.0.1:8790, only target of tunnels)
                                      │     /w/{worktree}[.{project}]/…  and  {worktree}.{public_domain}
                                      │     /_wg/oauth/callback
                                      │     allowlist → capture (redacted, bounded) → forward
                                      └─ tunnel provider ─► cloudflared | ngrok | external
```

Why the ingress is Go and not Caddy: capture, signed-state verification and allowlisting need to sit in
the request path. It uses the stdlib `httputil.ReverseProxy` (no custom proxy protocol), forwards only
to registry-resolved loopback ports, streams bodies byte-for-byte, and keeps `Host`/`X-Forwarded-*`
so signature schemes that sign the URL (Twilio) can be verified.

## 5. Phases

### Phase 1 — MVP (local identity & routing)
1. `internal/slug` — hostname sanitization + collision suffixes.
2. `internal/gitwt` — porcelain parsing, worktree resolution from any path, admin-dir identity.
3. `internal/config` — `wtg.yaml` (project) + `~/.config/worktree-gateway/config.yaml` (global).
4. `internal/registry` — model, persistence, hostname derivation, conflict detection.
5. `internal/proxy` — Caddy JSON generation, embedded + external providers, down/unknown pages.
6. `internal/daemon` — unix socket API, health loop, discovery loop, reload debounce.
7. `cmd/wtg` — `up`, `down`, `register`, `deregister`, `status`, `port`, `daemon`, `doctor`, `hooks worktrunk`.

### Phase 2
8. Multi-service templates & `wtg run` convenience runner.
9. `wtg env` / injected `WG_*` identity variables.
10. HTTPS via Caddy internal CA; `wtg trust` (smallstep truststore), `wtg hosts`.
11. Tunnel providers (cloudflared first, ngrok, external) and `wtg tunnel start|stop|status`.
12. Public exposure only for services with `public:` + explicit tunnel start.

### Phase 3
13. Ingress: webhook routing by path and host.
14. Bounded request log with redaction; `wtg requests`, `wtg replay` (non-idempotent needs `--yes`, marked `X-WG-Replay`).
15. OAuth callback routing with signed state; `wtg oauth state`, `WG_OAUTH_*` env.

## 6. Failure modes

| Failure | Handling |
|---|---|
| Stale registration | Health loop; PID death → remove; port closed → `down` → removed after `stale_after`. |
| Port changed after registration | Re-register replaces; `wtg run` always re-registers the actual port. |
| Service process died | PID check → removed; route answers 503 "service down" while `down`. |
| Worktree removed | Discovery loop + Worktrunk `pre-remove` hook → all services removed. |
| Duplicate worktree slug | Deterministic `-<hash4>` suffix, persisted. |
| Unsafe branch names | `slug.Sanitize` (length, charset, empty, unicode). |
| Tunnel disconnected | Provider process supervised with backoff; status shows `reconnecting`; quick-tunnel URL change is surfaced. |
| Callback to offline worktree | Ingress waits up to `hold` (10 s) for the service, then 503 + `Retry-After`; request is logged for replay. OAuth: explanatory 503 page. |
| Wildcard DNS unavailable | `wtg doctor` detects; `wtg hosts` prints explicit lines. |
| Local certificate issue | `wtg trust` / `wtg doctor` checks trust; plain HTTP listener always available. |
| Port already occupied | `wtg port` / `wtg run` probe before handing out; register verifies the port is listening (`--no-wait` to skip). |
| Same service registers twice | Latest wins (replace). |
| Worktree renamed / moved | Identity is the git admin dir; branch rename re-slugs, move updates path. |
| Webhook during restart | Hold-and-retry then 503 + `Retry-After`; captured for replay. |
| Replay of non-idempotent request | Requires `--yes`; header `X-WG-Replay: 1`, `X-WG-Replay-Of: <id>`. |
| Malicious request to internal service | Ingress resolves targets only from registry; no host/port from request; only loopback upstreams accepted at registration. |
| Tunnel exposing local-only service | Only services with `public:` config and matching path prefixes are reachable; everything else 404. |

## 7. Security requirements → implementation

* Public exposure opt-in: `public:` per service **and** `wtg tunnel start`.
* Route allowlist: ingress path-prefix allowlist per service.
* No arbitrary proxying: upstream is always `127.0.0.1|::1:<registered port>`.
* Callback integrity: HMAC-signed, expiring, single-use state with per-worktree keys.
* Request storage: redacted headers, bounded count/age/size, 0600 file, `capture.bodies: false` option.
* Admin surface: unix socket only; ingress binds 127.0.0.1.

## 8. Non-goals (enforced)

No worktree creation/switch/merge, no process supervision (no restart, no log mgmt), no tunnel/proxy
protocol, no secret management, no production ingress. Hermes Worker Runtime and State Capsule are not
dependencies; they can call `wtg register` / read `wtg env` like any other tool.

## Execution status

See README "Status" section and `git log`.
