<h1 align="center">Worktree Gateway</h1>

<p align="center">
  <b>Every Git worktree gets its own URL.</b><br>
  Local hostnames, webhooks and OAuth callbacks, routed to the right checkout.
</p>

<p align="center">
  <a href="https://github.com/chryzxc/worktree-gateway/releases/latest"><img alt="release" src="https://img.shields.io/github/v/release/chryzxc/worktree-gateway?sort=semver"></a>
  <a href="https://pkg.go.dev/github.com/chryzxc/worktree-gateway"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/chryzxc/worktree-gateway.svg"></a>
  <a href="https://goreportcard.com/report/github.com/chryzxc/worktree-gateway"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/chryzxc/worktree-gateway"></a>
  <a href="go.mod"><img alt="go" src="https://img.shields.io/github/go-mod/go-version/chryzxc/worktree-gateway"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/github/license/chryzxc/worktree-gateway"></a>
  <img alt="platforms" src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey">
</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#usage">Usage</a> ·
  <a href="docs/configuration.md">Configuration</a> ·
  <a href="docs/webhooks-and-oauth.md">Webhooks & OAuth</a> ·
  <a href="#faq">FAQ</a>
</p>

---

Working on three branches at once in three worktrees, or with three AI agents? Each one still wants `localhost:3000`, the same cookie domain, the same Stripe webhook URL and the same OAuth redirect URI.

`wtg` gives each worktree a stable identity and routes traffic to it:

```
~/src/shop            (main)          →  https://main.shop.localhost:8743   (also https://shop.localhost:8743)
~/src/shop.auth       (feature/auth)  →  https://feature-auth.shop.localhost:8743
  └─ api service                      →  https://api.feature-auth.shop.localhost:8743

Stripe        →  https://<tunnel>/w/feature-auth/webhooks/stripe  →  feature/auth worktree
GitHub login  →  https://<tunnel>/_wg/oauth/callback              →  whichever worktree started it
```

## Features

- 🌐 **A URL per worktree.** `{branch}.{project}.localhost` over HTTP and HTTPS. No `/etc/hosts` edits, no root.
- 🔢 **No port juggling.** Every service gets a stable, collision-free `$PORT`, and URLs stay the same across restarts.
- 🧩 **Many services per worktree.** `web`, `api`, `docs`… each with its own hostname and `WG_*_URL` env var.
- 🔄 **Follows git.** New, removed, renamed and moved worktrees are picked up automatically.
- 🪝 **Webhooks into any worktree.** One tunnel for all of them, with only the paths you allowlist exposed.
- 🔐 **One OAuth redirect URI for every worktree.** Signed state sends each login back to the right checkout.
- 🔁 **Webhook replay.** Inspect captured requests (credentials redacted) and re-send them to any worktree.
- 🧱 **Built on proven tools.** [Caddy](https://caddyserver.com) does the routing, and cloudflared or ngrok do the tunnelling. Pairs with [Worktrunk](https://worktrunk.dev).

## Install

**macOS / Linux (recommended)**

```sh
curl -fsSL https://raw.githubusercontent.com/chryzxc/worktree-gateway/main/install.sh | sh
```

This downloads the latest release, verifies its checksum and installs `wtg` to `~/.local/bin`. Set `WTG_INSTALL_DIR` to install somewhere else, or `WTG_VERSION=v0.1.0` to pin a version. The script is short; [read it](install.sh) before piping it to `sh` if you prefer.

<details>
<summary>Other methods</summary>

**Download a release manually.** Grab `wtg_<version>_<os>_<arch>.tar.gz` from [Releases](https://github.com/chryzxc/worktree-gateway/releases), extract it, and put `wtg` on your `PATH`.

**With Go 1.26+:**

```sh
go install github.com/chryzxc/worktree-gateway/cmd/wtg@latest
```

**From source:**

```sh
git clone https://github.com/chryzxc/worktree-gateway && cd worktree-gateway && make install
```

**Shell completion** (bash, zsh, fish, PowerShell):

```sh
echo 'source <(wtg completion zsh)' >> ~/.zshrc     # zsh
echo 'source <(wtg completion bash)' >> ~/.bashrc   # bash
wtg completion fish > ~/.config/fish/completions/wtg.fish
```

</details>

Check your setup:

```sh
wtg doctor
```

**Upgrading:** run the installer again, then `wtg daemon restart` so the background daemon picks up the new version. `wtg` warns you when the daemon and the CLI versions differ.

**Uninstalling:** `wtg daemon stop && wtg untrust`, then delete `~/.local/bin/wtg`, `~/.config/worktree-gateway` and `~/.local/state/worktree-gateway`.

## Quick start

```sh
cd ~/src/shop
wtg init          # writes wtg.yaml (guesses your dev command)
wtg run           # starts it on a stable $PORT and routes it
wtg open          # → https://main.shop.localhost:8743
wtg trust         # once: trust the local HTTPS certificate
```

Now add a second worktree. Any tool works:

```sh
git worktree add ../shop.auth -b feature/auth
cd ../shop.auth && wtg run
# wtg: feature-auth/web on port 24817 → https://feature-auth.shop.localhost:8743
```

Both run side by side, each on its own URL:

```console
$ wtg status
WORKTREE      BRANCH        SERVICE  STATUS  UPSTREAM         URL
main (main)   main          web      up      127.0.0.1:21853  https://main.shop.localhost:8743
feature-auth  feature/auth  web      up      127.0.0.1:24817  https://feature-auth.shop.localhost:8743
```

Remove the worktree with `git worktree remove` and its routes disappear.

> [!TIP]
> Your dev server must listen on `$PORT`. Most frameworks (Next.js, Express, Rails, Django, Phoenix…) read it automatically. Vite needs `vite --port $PORT`.

## Usage

### Run services

```sh
wtg run                           # the default service, command from wtg.yaml
wtg run api -- go run ./cmd/api   # any command; gets PORT and WG_* env
wtg register db-admin --port 8081 # route something you started yourself
```

`wtg run` injects identity env vars. Apps use them to build absolute URLs and to find their sibling services:

```sh
WG_URL=https://feature-auth.shop.localhost:8743
WG_API_URL=https://api.feature-auth.shop.localhost:8743
WG_WORKTREE=feature-auth   WG_BRANCH=feature/auth   PORT=24817
```

Use `eval "$(wtg env)"` to load the same variables into your shell. The [full list](docs/configuration.md#environment-variables) is in the configuration docs.

### Several services

```yaml
# wtg.yaml
project: shop
services:
  web:
    command: npm run dev
  api:
    command: go run ./cmd/api
    health: /healthz
```

`web` is served at `feature-auth.shop.localhost` and `api` at `api.feature-auth.shop.localhost`. All options are in the [configuration reference](docs/configuration.md).

### Webhooks

```yaml
services:
  web:
    public:
      paths: [/webhooks]      # the only paths reachable from the internet
```

```sh
wtg tunnel start              # needs cloudflared (default) or ngrok
wtg status                    # public: https://abc.trycloudflare.com/w/feature-auth
```

Point Stripe at `https://abc.trycloudflare.com/w/feature-auth/webhooks/stripe`. Then inspect and replay what arrived:

```sh
wtg requests
wtg replay req_1a2b3c4d --yes
wtg replay req_1a2b3c4d --to main --yes   # send it to another worktree
```

### OAuth callbacks

Register **one** redirect URI with your provider. The gateway sends each callback back to the worktree that started the login, using signed state. See [docs/webhooks-and-oauth.md](docs/webhooks-and-oauth.md#oauth-callbacks) for the Node and Go snippets.

### Worktrunk

```sh
wtg hooks worktrunk --write   # new worktrees register themselves; removal cleans up
```

### Command reference

| Command | Description |
|---|---|
| `wtg init` | Create a starter `wtg.yaml` |
| `wtg run [svc] [-- cmd…]` | Run a dev server with `$PORT` and identity env, routed while it runs |
| `wtg open [svc]` | Open the worktree's URL in the browser |
| `wtg status [-a] [--json]` | Worktrees, services, health and URLs |
| `wtg up` / `wtg down [--forget]` | Register a worktree or take its routes down (usually automatic) |
| `wtg register <svc> --port N` / `wtg deregister <svc>` | Route a process you started yourself |
| `wtg env [svc]` / `wtg port [svc]` | Print identity exports or the stable port |
| `wtg tunnel start\|stop\|status` | Opt-in public exposure |
| `wtg requests [id]` / `wtg replay <id>` | Inspect and replay webhook traffic |
| `wtg oauth state --callback /path` | Mint a signed OAuth state |
| `wtg trust` / `wtg untrust` | Install or remove the local HTTPS CA |
| `wtg doctor` · `wtg hosts` · `wtg hooks worktrunk` | Diagnostics and integrations |
| `wtg daemon start\|stop\|restart\|status` | Background daemon (auto-started) |
| `wtg completion <shell>` | Shell completion script |

Run `wtg <command> --help` for flags and examples.

## How it works

```
 wtg CLI ──unix socket──► wtg daemon ─┬─ registry        projects → worktrees → services
                                      ├─ discovery       git worktree list (add/remove/rename/move)
                                      ├─ health checks   process + port / HTTP probe
                                      ├─ Caddy ─────────► :8780 http · :8743 https (local CA)
                                      └─ ingress :8790 ◄─ cloudflared / ngrok   (only when you start a tunnel)
```

A worktree's identity comes from git itself, so moving the worktree or renaming its branch doesn't break it. Every change rebuilds Caddy's config and reloads it with no downtime. The ingress only forwards allowlisted paths to registered loopback services. The design and the trade-offs are in [docs/PLAN.md](docs/PLAN.md).

## Security

`wtg` is a local development tool and is safe by default:

- Nothing is reachable from outside until you run `wtg tunnel start`. After that, only the paths in `public.paths` are reachable.
- It only forwards to registered services on loopback, so it cannot act as an open proxy or be used for SSRF.
- OAuth state is HMAC-signed, expires, can only be used once, and must match a path you allowlisted.
- The request log is size- and time-bounded, redacts credentials, is readable only by you, and can skip bodies.
- The control API is a user-only unix socket. There is no root access, `/etc/hosts` is never edited, and the CA is trusted only when you run `wtg trust`.

The details and how to report a vulnerability are in [SECURITY.md](SECURITY.md).

## FAQ

<details>
<summary><b>Why <code>wtg</code> and not <code>wg</code>?</b></summary>

`wg` is WireGuard's CLI. The environment variables keep the `WG_` prefix.
</details>

<details>
<summary><b>Why port 8743 instead of 443?</b></summary>

Binding ports below 1024 needs root, and `wtg` never asks for it. To use `https://feature-auth.shop.localhost` without a port, run your own Caddy on 443 and set `proxy.provider: caddy-admin`. `wtg` then pushes its routes to that Caddy instead.
</details>

<details>
<summary><b>A tool can't resolve <code>*.localhost</code>.</b></summary>

Browsers and most systems resolve it to loopback (RFC 6761). For tools that don't, `wtg hosts` prints `/etc/hosts` lines you can add, or use `curl --resolve host:8743:127.0.0.1`.
</details>

<details>
<summary><b>How does this compare to Portless, Portree or Worktrunk?</b></summary>

[Portless](https://github.com/vercel-labs/portless) gives apps `*.localhost` names with its own proxy. Portree supervises processes per worktree. [Worktrunk](https://worktrunk.dev) creates and manages worktrees, and works well with `wtg`.

`wtg` treats a worktree, with all of its services, as one identity tracked from git. It adds external webhook and OAuth routing plus replay, and it builds on Caddy rather than its own proxy. The full comparison is in [docs/PLAN.md](docs/PLAN.md#1-audit-of-existing-tools-september-2026).
</details>

<details>
<summary><b>Does it restart my crashed dev server?</b></summary>

No. `wtg` is not a process supervisor. When a service dies, its route shows "service not running", and public webhook requests are held briefly (then answered 503) so providers retry.
</details>

<details>
<summary><b>Something isn't working.</b></summary>

Run `wtg doctor`, then check `~/.local/state/worktree-gateway/daemon.log`. Common fixes:

| Symptom | Fix |
|---|---|
| Browser certificate warning | `wtg trust` (restart Firefox) |
| `the running daemon is vX but this binary is vY` | `wtg daemon restart` after upgrading |
| `address already in use` | change `http_port` / `https_port` / `ingress.port` in `~/.config/worktree-gateway/config.yaml` |
| Service stuck on `starting` | your server isn't listening on `$PORT` |
| Webhook 404 through the tunnel | the path isn't in `public.paths`, or the worktree name is wrong (`wtg status`) |
| Replayed webhook fails its signature check | expected: signature headers are redacted, so detect `X-Wg-Replay: 1` in test mode |
</details>

## Roadmap

- [x] Local identity and routing, health, discovery, Worktrunk hooks
- [x] Multi-service, env injection, HTTPS, tunnels, opt-in exposure
- [x] Webhook routing, request log, replay, signed OAuth callbacks
- [ ] Homebrew tap
- [ ] Windows support (it builds, but is untested)
- [ ] `wtg ui`: a local dashboard for requests and routes

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the dev setup (`make build test lint`) and the project's non-goals. Changes are tracked in [CHANGELOG.md](CHANGELOG.md).

## License

[MIT](LICENSE) © 2026 Christian Rey Villablanca
