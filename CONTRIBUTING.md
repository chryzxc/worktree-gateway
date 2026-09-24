# Contributing

Thanks for your interest! Start with [docs/PLAN.md](docs/PLAN.md). It explains the scope and the **non-goals**. Changes that would turn `wtg` into a process supervisor, secret manager, worktree manager or new proxy/tunnel will be declined.

## Development

Requirements: Go (the version in `go.mod`) and git.

```sh
make build     # bin/wtg
make test      # go test -race ./...  (boots embedded Caddy and real git repos)
make lint      # gofmt + go vet (incl. GOOS=windows)
```

Test the binary without touching your real state:

```sh
export WTG_HOME=$(mktemp -d) WTG_SOCKET=/tmp/wtg-dev.sock
printf 'http_port: 18780\nhttps_port: 18743\ningress:\n  port: 18790\n' > $WTG_HOME/config.yaml
./bin/wtg up && ./bin/wtg status
./bin/wtg daemon stop
```

Keep unix socket paths short, because macOS limits them to about 104 bytes. Tests use `os.MkdirTemp("", "wtg")` rather than `t.TempDir()`.

## Layout

| Package | Responsibility |
|---|---|
| `cmd/wtg` | CLI (cobra) |
| `internal/daemon` | daemon, API handlers, discovery/health loops, env, ports |
| `internal/registry` | persisted identity model, slugs, route table |
| `internal/gitwt` | `git worktree` / `rev-parse` wrappers |
| `internal/proxy` | Caddy JSON config + embedded / admin-API providers |
| `internal/ingress` | public ingress, allowlist, OAuth callback, replay |
| `internal/capture` | bounded, redacted request log |
| `internal/oauth` | signed state |
| `internal/tunnel` | cloudflared / ngrok / external supervisor |
| `internal/config`, `internal/slug`, `internal/api` | config, hostname labels, socket API types + client |

## Pull requests

- Add tests for behaviour changes. Security-relevant code (ingress, oauth, capture, registry loopback checks) needs negative tests.
- Run `make lint test` before pushing.
- Update `CHANGELOG.md` under *Unreleased*.
- Keep commits focused, with messages in the imperative mood.
