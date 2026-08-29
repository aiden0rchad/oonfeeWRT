# Put TLS in front of the controller

oonfeeWRT v0.1.2 serves HTTP and WebSocket traffic but has no native TLS listener. Keep it on loopback and let a trusted reverse proxy terminate HTTPS.

> **Outcome:** Browsers connect to a trusted `https://` address while the controller remains reachable only as `127.0.0.1:8080` on its host.

## Prerequisites

- A working oonfeeWRT controller bound to `127.0.0.1:8080`.
- A reverse proxy on the same host, or another host that reaches a deliberately isolated controller listener.
- An internal DNS name and certificate trusted by every administrator browser.
- Proxy support for WebSocket upgrades and `X-Forwarded-Proto`.

**Write impact:** This changes only controller-host proxy and DNS/certificate configuration. It does not contact or change a router.

## Why TLS matters

At-rest encryption protects saved credentials in the controller database. It does not protect:

- a password or WLAN key while a browser submits it;
- an authenticated session cookie in transit;
- data in daemon memory;
- topology, client, or configuration details crossing the network.

Use direct HTTP only on host loopback or a deliberately trusted, isolated management network. Never expose port 8080 directly to the Internet.

## 1. Confirm the backend is loopback-only

For a standalone process, use:

```sh
oonfeewrtd \
  -data-dir /absolute/path/to/data \
  -passphrase-file /absolute/path/to/passphrase \
  -listen 127.0.0.1:8080
```

The supplied Compose file already maps:

```text
127.0.0.1:8080:8080
```

Verify locally:

```sh
curl --fail http://127.0.0.1:8080/healthz
```

## 2. Configure Caddy

A minimal Caddy site is:

```text
oonfeewrt.example.internal {
    reverse_proxy 127.0.0.1:8080
}
```

Replace `oonfeewrt.example.internal` with the DNS name in your trusted certificate. Caddy preserves WebSocket upgrades and supplies `X-Forwarded-Proto: https` by default.

Reload Caddy through the installation method you already use. oonfeeWRT does not install or manage the proxy.

## 3. Verify HTTPS

From a client that trusts the certificate:

```sh
curl --fail https://oonfeewrt.example.internal/healthz
```

Expected output:

```text
ok
```

Then:

1. Open `https://oonfeewrt.example.internal`.
2. Sign in.
3. Open a device detail screen and leave it open long enough to receive live updates. This verifies the `/api/v1/live` WebSocket path as well as ordinary REST requests.
4. Confirm the browser reports a trusted certificate and no mixed-content warning.

When `X-Forwarded-Proto` is `https`, oonfeeWRT marks both session and CSRF cookies `Secure`. The session cookie is also `HttpOnly` and `SameSite=Strict`.

## Proxy requirements

If you use a proxy other than Caddy, preserve:

- the original `Host` value;
- WebSocket `Upgrade` and `Connection` behavior;
- `X-Forwarded-Proto: https`;
- request and response bodies without caching authenticated API responses.

The API sends `Cache-Control: no-store`. Do not override it with a shared cache. Hashed UI assets may be cached; `index.html` must remain revalidated so upgrades do not leave browsers referencing deleted asset names.

## Troubleshooting and recovery

### Sign-in succeeds over HTTP but fails over HTTPS

Confirm the proxy sends `X-Forwarded-Proto: https` and does not rewrite or strip `Set-Cookie` headers. Clear stale cookies for the controller origin after correcting the proxy.

### Pages load but live device data stops

The REST path works but the WebSocket upgrade likely does not. Verify proxy WebSocket support for `/api/v1/live`. Caddy's `reverse_proxy` handles this without extra directives.

### The proxy returns 502 or connection refused

When the proxy and controller share a host, run:

```sh
curl --fail http://127.0.0.1:8080/healthz
```

For a proxy on another host, probe the controller's deliberately isolated address instead of `127.0.0.1`. If the backend probe fails, fix the controller route first. If it succeeds, check that the proxy targets the correct network namespace. A proxy in another container cannot use its own `127.0.0.1` to reach the oonfeeWRT container; connect them through a deliberate private container network or proxy the host mapping.

### The browser reports an untrusted certificate

Install a certificate issued by a CA trusted by the administrator devices. Do not train users to bypass certificate warnings for a controller that receives router and account credentials.

### The controller is intentionally on a management LAN

Bind only to the specific trusted interface address, protect port 8080 with host/network firewall policy, and proxy it over a route you control. A wildcard `:8080` listener exposes every host interface and is not the recommended default.

## Recovery

If the proxy configuration fails, the loopback backend is unchanged. Access it locally at `http://127.0.0.1:8080`, repair the proxy, and verify HTTPS again. Do not broaden the controller bind merely to work around a proxy routing error.

## Next steps

- [Configure controller accounts and roles](../operations/accounts.md)
- [Back up and restore the controller](../operations/backups.md)
- [Review routine maintenance](../operations/maintenance.md)
