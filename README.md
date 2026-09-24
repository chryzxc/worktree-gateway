# Worktree Gateway (`wtg`)

Worktree Gateway gives each Git worktree a stable runtime identity. It routes local traffic to that worktree's services, and routes external traffic too if you turn that on.

```
feature/auth worktree    →  https://feature-auth.shop.localhost:8743
  └─ api service         →  https://api.feature-auth.shop.localhost:8743
Stripe webhook (tunnel)  →  https://<tunnel>/w/feature-auth/webhooks/stripe
OAuth callback           →  https://<tunnel>/_wg/oauth/callback  →  the worktree that started the login
```

It does **not** manage worktrees; use [Worktrunk](https://worktrunk.dev) or plain `git worktree` for that. It does **not** implement a proxy or a tunnel either. Local routing is done by [Caddy](https://caddyserver.com), embedded or your own. Public exposure uses cloudflared, ngrok or any tunnel you already run.

See [docs/PLAN.md](docs/PLAN.md) for the tool audit, design decisions and roadmap.

## Install

```sh
go install github.com/chryzxc/worktree-gateway/cmd/wtg@latest
```

The binary is named `wtg`, not `wg`, because `wg` is WireGuard's CLI. Environment variables keep the `WG_` prefix.

## Quick start

```sh
cd ~/src/shop                 # any git repo
wtg up                        # registers this worktree (daemon auto-starts)
wtg run web -- npm run dev    # allocates PORT, injects WG_* env, routes while running
wtg status
# WORKTREE     BRANCH  SERVICE  STATUS  UPSTREAM         URL
# main (main)  main    web      up      127.0.0.1:24817  https://main.shop.localhost:8743

wtg trust                     # once: trust the local CA for HTTPS (explicit, may prompt)
```

Create a second worktree with `git worktree add ../shop.auth -b feature/auth`, or with `wt switch -c feature/auth`. Run `wtg up` there and it gets `feature-auth.shop.localhost` next to the first one. The main worktree also answers on the bare project domain, `shop.localhost`.

`*.localhost` resolves to loopback in browsers and on most systems (RFC 6761), so nothing touches `/etc/hosts`. `wtg hosts` prints lines you can add yourself for tools that don't resolve it. The gateway listens on unprivileged loopback ports: 8780 for HTTP, 8743 for HTTPS and 8790 for ingress. It needs no root.

Services you already start some other way can be registered directly:

```sh
wtg register api --port 4000 [--pid 1234]   # with --pid, removed when that process exits
wtg deregister api
```

## Worktrunk integration

```sh
wtg hooks worktrunk --write     # appends to .config/wt.toml
```

```toml
[post-start]
gateway = "wtg up --path {{ worktree_path }}"

[pre-remove]
gateway = "wtg down --path {{ worktree_path }} --forget"
```

Worktrunk's `hash_port` range is 10000–19999. The gateway allocates ports from 20000–29999 so the two never collide. Neither integration is required: the daemon also runs `git worktree list` every 10s. It adds new worktrees, drops removed ones, re-slugs renamed branches and follows `git worktree move`.

## Identity model

| Thing | Identity | Stable across |
|---|---|---|
| Project | `sha256(git common dir)` | clones at other paths get their own identity |
| Worktree | git admin dir (`.git/worktrees/<id>`; `.` for main) | `git worktree move`, branch renames |
| Hostname label | branch → `[a-z0-9-]`, ≤63 chars | pinned with `wtg up --name x` or `WG_WORKTREE=x` |

A detached HEAD uses the directory name. If two branches sanitize to the same label, the later worktree gets a `-abcd` hash suffix. If two projects claim the same hostname, the earlier registration keeps it and `wtg status` shows the conflict.

## Environment injected by `wtg run` / `wtg env`

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

## Project config: `wtg.yaml`

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

## External traffic (Phase 2/3)

```sh
wtg tunnel start                      # cloudflared quick tunnel by default
wtg status                            # shows https://<random>.trycloudflare.com/w/<worktree>
```

All worktrees share one tunnel, and the tunnel points only at the gateway ingress. Requests route by path, `/w/<worktree>[.<project>]/<path>`, or by host, `<worktree>.<public_domain>`, if you have a wildcard hostname.

The ingress only forwards when all of these are true:

1. A tunnel was started. Before that, the ingress refuses everything.
2. The worktree exists, and the target service lists a matching prefix in `public.paths`. The longest prefix wins, matching whole segments. The path is normalised before matching, so `..` and `%2e%2e` cannot escape the allowlist.
3. The upstream is a registered loopback service. The gateway never proxies to arbitrary hosts, so there is no SSRF and no open proxy.

The prefix `/w/<worktree>` is stripped; set `public.strip_prefix: false` to keep it. The original body is forwarded byte-for-byte, and the Host and signature headers are kept, so Stripe, GitHub and Slack signature checks still work. `X-Forwarded-Prefix` carries the stripped prefix. If the service is restarting, the request is held for up to `ingress.hold` and then gets a 503 with `Retry-After`, so providers retry.

### Request log and replay

```sh
wtg requests                     # recent public requests (source detected: stripe, github, slack…)
wtg requests req_1a2b3c4d        # full detail
wtg replay req_1a2b3c4d --yes    # POST needs --yes
wtg replay req_1a2b3c4d --to feature-other --yes
wtg requests --clear
```

The log is bounded to 500 entries and 7 days, with bodies capped at 1 MiB. It is a 0600 file in the state dir. Credential headers are redacted: `Authorization`, `Cookie`, signature and token headers, plus any you add. Credential-like query values are redacted too.

A replayed request carries `X-Wg-Replay: 1` and `X-Wg-Replay-Of: <id>`. Redacted headers are **not** re-sent, so a signature check has to recognise replays, for example with a test-mode bypass. A request whose body was not captured, or was truncated, is never replayed.

### OAuth callbacks

Register one redirect URI with the provider: `<public url>/_wg/oauth/callback`, which `WG_OAUTH_CALLBACK_URL` holds. Without a tunnel it is `https://<project domain>/_wg/oauth/callback`, which works for providers that accept localhost callbacks. Then send each worktree's login with a **signed state**:

```sh
wtg oauth state --callback /auth/callback --state <your-own-state>
```

When the provider redirects back, the gateway checks the state. The HMAC must be valid, it must not be expired, it must not have been used before, and its path must be listed in `oauth_callbacks`. The gateway then redirects the browser to `<worktree URL>/auth/callback?code=…&state=<your-own-state>`. For `response_mode=form_post` it re-posts the form instead. Unsigned, tampered, expired and replayed states are rejected, so the callback endpoint cannot be used as an open redirect.

Apps usually mint the state themselves using `WG_OAUTH_STATE_KEY`. That key is per-worktree and can only sign states for its own worktree.

Format: `base64url(json) + "." + base64url(HMAC-SHA256(key, base64url(json)))`. The JSON is:

```json
{"v":1,"p":"<WG_PROJECT>","w":"<WG_WORKTREE>","s":"<service>","path":"/auth/callback","st":"<your state>","exp":<unix>,"n":"<random nonce>"}
```

```js
import crypto from "node:crypto";
export function wgState(appState, path = "/auth/callback", ttlSec = 900) {
  const b64 = (b) => Buffer.from(b).toString("base64url");
  const body = b64(JSON.stringify({
    v: 1, p: process.env.WG_PROJECT, w: process.env.WG_WORKTREE, s: process.env.WG_SERVICE,
    path, st: appState, exp: Math.floor(Date.now() / 1000) + ttlSec, n: crypto.randomBytes(12).toString("base64url"),
  }));
  const mac = crypto.createHmac("sha256", Buffer.from(process.env.WG_OAUTH_STATE_KEY, "hex")).update(body).digest();
  return `${body}.${b64(mac)}`;
}
```

```go
func wgState(appState, path string) string {
	key, _ := hex.DecodeString(os.Getenv("WG_OAUTH_STATE_KEY"))
	nonce := make([]byte, 12)
	rand.Read(nonce)
	body, _ := json.Marshal(map[string]any{"v": 1, "p": os.Getenv("WG_PROJECT"), "w": os.Getenv("WG_WORKTREE"),
		"s": os.Getenv("WG_SERVICE"), "path": path, "st": appState,
		"exp": time.Now().Add(15 * time.Minute).Unix(), "n": base64.RawURLEncoding.EncodeToString(nonce)})
	head := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(head))
	return head + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
```

When the app gets the callback, it checks `state` against its own session value as usual.

## Security

| Requirement | How |
|---|---|
| Public exposure is opt-in | The ingress forwards nothing until `wtg tunnel start`. Only `public.paths` prefixes are reachable. Local-only services have no public route. |
| No SSRF / open proxy | Upstreams must be loopback (`127.0.0.0/8`, `::1`) and registered. Request data never picks the target host. Paths are normalised before matching. |
| Signed callback state | HMAC-SHA256 with a per-worktree key from a 0600 master key. Expiry and single-use nonces are enforced, and the callback path must be allowlisted. |
| Bounded, redacted storage | The capture log has entry, age and body caps, redacts headers and query values, is a 0600 file, can stop capturing bodies (`capture.bodies: false`) or be disabled entirely. OAuth callbacks are never captured. |
| Admin surface is local | The control API is a 0600 unix socket. Embedded Caddy's admin API is disabled. All listeners must bind loopback. |
| No privileged changes | No root and no `/etc/hosts` edits. The CA is trusted only by an explicit `wtg trust`. |

## Commands

| | |
|---|---|
| `wtg up [--name N] [--path P]` | register a worktree (idempotent; also unparks it) |
| `wtg down [--forget]` | park the worktree's routes, or with `--forget` drop its identity |
| `wtg run [svc] [-- cmd…]` | run a dev server with PORT and identity env; routed while it lives |
| `wtg register svc --port N [--pid P]` / `wtg deregister svc` | route an existing process |
| `wtg status [-a] [--json]` | worktrees, services, health, URLs |
| `wtg port [svc]` / `wtg env [svc]` | stable port / identity exports |
| `wtg tunnel start\|stop\|status [--provider]` | opt-in public exposure |
| `wtg requests [id] [-w wt] [--clear]` / `wtg replay id [--to wt] [--yes]` | request log & replay |
| `wtg oauth state --callback /path [--state s]` | mint signed OAuth state |
| `wtg trust` / `wtg untrust` | install/remove the local CA |
| `wtg doctor`, `wtg hosts`, `wtg hooks worktrunk [--write]` | diagnostics & integration |
| `wtg daemon run\|start\|stop\|status` | daemon lifecycle (normally auto-started) |

## Non-goals

`wtg` is not a process supervisor: `wtg run` does not restart anything. It is not an env or secret manager, and not production ingress. It also does not depend on Hermes or State Capsule, which stay independent.

## Status

| Phase | Scope | State |
|---|---|---|
| MVP | git discovery, identity model, `.localhost` hostnames, dynamic Caddy routing, status, safe deregistration, Worktrunk hooks | ✅ implemented, tested |
| 2 | multi-service templates, env injection, HTTPS (Caddy internal CA), tunnels (cloudflared/ngrok/external), opt-in exposure | ✅ implemented; cloudflared/ngrok parsing unit-tested with fake binaries |
| 3 | webhook routing, bounded request log, replay, signed OAuth callback routing | ✅ implemented, tested |

Not yet done: packaged releases, a Windows CI run (the code builds for Windows but is untested there), and a live end-to-end test against a real cloudflared or ngrok account.

## Development

```sh
make test      # go test -race ./...
make build     # ./bin/wtg
```

Test the binary against a scratch state dir with `WTG_HOME=$(mktemp -d) WTG_SOCKET=/tmp/wtg-dev.sock ./bin/wtg …`.

## License

MIT
