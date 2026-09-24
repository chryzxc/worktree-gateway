# Security Policy

Worktree Gateway is a **local development tool**. If you turn on a tunnel, it receives traffic from the internet, so its security boundaries matter. [README § Security](README.md#security) describes them.

## Security model

| Requirement | How |
|---|---|
| Public exposure is opt-in | The ingress forwards nothing until `wtg tunnel start`. Only `public.paths` prefixes are reachable. Local-only services have no public route. |
| No SSRF / open proxy | Upstreams must be loopback (`127.0.0.0/8`, `::1`) and registered. Request data never picks the target host. Paths are normalised before matching. |
| Signed callback state | HMAC-SHA256 with a per-worktree key from a 0600 master key. Expiry and single-use nonces are enforced, and the callback path must be allowlisted. |
| Bounded, redacted storage | The capture log has entry, age and body caps, redacts headers and query values, is a 0600 file, can stop capturing bodies (`capture.bodies: false`) or be disabled entirely. OAuth callbacks are never captured. |
| Admin surface is local | The control API is a 0600 unix socket. Embedded Caddy's admin API is disabled. All listeners must bind loopback. |
| No privileged changes | No root and no `/etc/hosts` edits. The CA is trusted only by an explicit `wtg trust`. |

Full design notes: [docs/PLAN.md](docs/PLAN.md#7-security-requirements--implementation).

## Supported versions

| Version | Supported |
|---|---|
| latest `0.x` release | ✅ |
| older | ❌ |

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's
[private vulnerability reporting](https://github.com/chryzxc/worktree-gateway/security/advisories/new),
or email the maintainer at the address on the GitHub profile.

Please include:
- the version (`wtg version`) and OS
- the configuration involved (`wtg.yaml` and the global config, with secrets removed)
- reproduction steps and the impact you observed

You can expect an acknowledgement within a few days. Fixes are released as patch versions and credited in the changelog, unless you prefer to stay anonymous.

## In scope

- Reaching a service or path that is not in `public.paths` through the ingress
- Making the gateway connect to a non-loopback or unregistered upstream (SSRF / open proxy)
- Forging, replaying or redirecting OAuth state, or turning `/_wg/oauth/callback` into an open redirect
- Credentials leaking into the request log despite redaction
- Reaching the control socket or Caddy admin from another user or over the network

## Out of scope

- Vulnerabilities in the apps you route to, or in cloudflared, ngrok or Caddy themselves (report those upstream)
- Anything that requires running `wtg` in production. It is not production ingress.
