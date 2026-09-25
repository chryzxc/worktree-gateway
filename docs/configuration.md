# Configuration reference

`wtg` works with no configuration at all. Each worktree gets one `web` service at `{worktree}.{project}.localhost`. Add a `wtg.yaml` when you need more services, fixed ports, public paths or OAuth callbacks. `wtg init` writes a starter file for you.

- [Project config (`wtg.yaml`)](#project-config-wtgyaml)
- [Global config](#global-config)
- [Environment variables](#environment-variables)
- [Identity model](#identity-model)
- [Files and paths](#files-and-paths)

## Project config (`wtg.yaml`)

Optional. Place it at the worktree root; `.wtg.yaml` and `.yml` also work. See [`examples/wtg.yaml`](examples/wtg.yaml).

```yaml
project: shop                 # default: main worktree directory name
domain: shop.localhost        # default: {project}.localhost
default_service: web          # default: "web" if defined, else the only service
main_alias: true              # main worktree also answers on {domain}
services:
  web:
    command: npm run dev -- --port $PORT   # used by `wtg run web`
    port: auto                              # or a fixed number
    health: /healthz                        # optional; "up" = non-5xx
    public:
      paths: [/webhooks]                    # ONLY these prefixes are reachable publicly
    oauth_callbacks: [/auth/callback]
  api:
    hostname: "api.{worktree}.{domain}"     # default for non-default services
```

Hostname templates may use `{worktree}`, `{project}`, `{domain}` and `{service}`, and must contain `{worktree}`.

## Global config

The global config lives at `~/.config/worktree-gateway/config.yaml` (`WTG_CONFIG` or `WTG_HOME` override it). State lives at `~/.local/state/worktree-gateway` (override with `WTG_HOME`).

```yaml
listen_host: 127.0.0.1        # must be loopback
http_port: 8780
https_port: 8743
https: true
proxy:
  provider: embedded          # or caddy-admin (POST config to an existing Caddy)
  admin_url: http://localhost:2019
ingress:
  port: 8790                  # the only thing a tunnel points at
  hold: 10s                   # hold public requests while a service restarts
tunnel:
  provider: cloudflared       # cloudflared | ngrok | external
  public_url: ""              # required for external / named tunnels
  public_domain: ""           # enables <worktree>.<public_domain> host routing
  name: ""                    # named cloudflared tunnel
capture:
  enabled: true
  bodies: true                # false: metadata only
  max_entries: 500
  max_age: 168h
  max_body: 1048576
oauth:
  state_ttl: 15m
health_interval: 2s
discovery_interval: 10s
stale_after: 10m              # unhealthy registrations without a PID are dropped
port_range: [20000, 29999]
```

## Environment variables

Injected by `wtg run` and printed by `wtg env`.

Only gateway identity is injected. Application config and secrets stay in your own tooling.

| Variable | Example |
|---|---|
| `WG_GATEWAY` | `1` |
| `WG_PROJECT`, `WG_PROJECT_DOMAIN` | `shop`, `shop.localhost` |
| `WG_WORKTREE`, `WG_WORKTREE_PATH`, `WG_BRANCH` | `feature-auth`, `/src/shop.auth`, `feature/auth` |
| `WG_URL` | default service URL |
| `WG_<SVC>_URL`, `WG_<SVC>_HOST` | every service of this worktree (e.g. `WG_API_URL`) |
| `WG_SERVICE`, `WG_SERVICE_URL`, `PORT` | the service being run |
| `WG_PUBLIC_URL` | public base URL while a tunnel runs |
| `WG_OAUTH_CALLBACK_URL`, `WG_OAUTH_STATE_KEY` | when `oauth_callbacks` is configured |

`eval "$(wtg env)"` exports the same variables into your shell.

## Identity model

| Thing | Identity | Stable across |
|---|---|---|
| Project | `sha256(git common dir)` | clones at other paths get their own identity |
| Worktree | git admin dir (`.git/worktrees/<id>`; `.` for main) | `git worktree move`, branch renames |
| Hostname label | branch → `[a-z0-9-]`, ≤63 chars | pinned with `wtg up --name x` or `WG_WORKTREE=x` |

A detached HEAD uses the directory name. If two branches sanitize to the same label, the later worktree gets a `-abcd` hash suffix. If two projects claim the same hostname, the earlier registration keeps it and `wtg status` shows the conflict.

## Files and paths

| Path | Override | Contents |
|---|---|---|
| `~/.config/worktree-gateway/config.yaml` | `WTG_CONFIG`, `WTG_HOME` | global config |
| `~/.local/state/worktree-gateway/` | `WTG_HOME`, `XDG_STATE_HOME` | registry, request log, OAuth key, Caddy data + CA, logs |
| `<state>/wtg.sock` | `WTG_SOCKET` | control socket (0600) |
| `<state>/daemon.log`, `<state>/caddy.log` | | daemon and Caddy logs |
| `<state>/daemon.lock` | | held by the running daemon; prevents a second one from starting |

Set `WTG_NO_AUTOSTART=1` to stop the CLI from auto-starting the daemon.
