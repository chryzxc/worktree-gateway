# Webhooks, tunnels & OAuth callbacks

Everything on this page is **opt-in**. Nothing is reachable from outside your machine until you run `wtg tunnel start`, and even then only the paths you list under `public.paths`.

## Tunnels

Install [cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/) (`brew install cloudflared`) or [ngrok](https://ngrok.com/download). Then:

```sh
wtg tunnel start                      # cloudflared quick tunnel by default
wtg tunnel start --provider ngrok
wtg status                            # shows https://<random>.trycloudflare.com/w/<worktree>
```

All worktrees share one tunnel, and the tunnel points only at the gateway ingress. Requests route by path, `/w/<worktree>[.<project>]/<path>`, or by host, `<worktree>.<public_domain>`, if you have a wildcard hostname.

The ingress only forwards when all of these are true:

1. A tunnel was started. Before that, the ingress refuses everything.
2. The worktree exists, and the target service lists a matching prefix in `public.paths`. The longest prefix wins, matching whole segments. The path is normalised before matching, so `..` and `%2e%2e` cannot escape the allowlist.
3. The upstream is a registered loopback service. The gateway never proxies to arbitrary hosts, so there is no SSRF and no open proxy.

The prefix `/w/<worktree>` is stripped; set `public.strip_prefix: false` to keep it. The original body is forwarded byte-for-byte, and the Host and signature headers are kept, so Stripe, GitHub and Slack signature checks still work. `X-Forwarded-Prefix` carries the stripped prefix. If the service is restarting, the request is held for up to `ingress.hold` and then gets a 503 with `Retry-After`, so providers retry.

## Request log and replay

```sh
wtg requests                     # recent public requests (source detected: stripe, github, slack…)
wtg requests req_1a2b3c4d        # full detail
wtg replay req_1a2b3c4d --yes    # POST needs --yes
wtg replay req_1a2b3c4d --to feature-other --yes
wtg requests --clear
```

The log is bounded to 500 entries and 7 days, with bodies capped at 1 MiB. It is a 0600 file in the state dir. Credential headers are redacted: `Authorization`, `Cookie`, signature and token headers, plus any you add. Credential-like query values are redacted too.

A replayed request carries `X-Wg-Replay: 1` and `X-Wg-Replay-Of: <id>`. Redacted headers are **not** re-sent, so a signature check has to recognise replays, for example with a test-mode bypass. A request whose body was not captured, or was truncated, is never replayed.

## OAuth callbacks

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

## Tunnel providers

| Provider | Config | Notes |
|---|---|---|
| `cloudflared` (default) | nothing | Quick tunnel with a random `*.trycloudflare.com` URL that changes on restart |
| `cloudflared` named | `tunnel.name`, `tunnel.public_url` | Stable hostname. Add a wildcard DNS record and set `public_domain` for host routing |
| `ngrok` | optional `tunnel.public_url` (reserved domain), `tunnel.args` | Uses your ngrok auth token |
| `external` | `tunnel.public_url` | You run any tunnel yourself and point it at `http://127.0.0.1:8790` |

The daemon supervises the tunnel process and restarts it with backoff if it fails.
