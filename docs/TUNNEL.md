# Reaching it from the internet

The server binds `127.0.0.1` and should stay there. To reach it remotely you need
something that terminates locally and reaches your Mac without opening inbound ports.

**A Cloudflare Tunnel is the recommended route** — no port forwarding, no static IP,
nothing exposed on your router. This guide uses it. Any equivalent (Tailscale Funnel,
an SSH reverse tunnel, a VPN) works on the same principle.

> Only the signaling handshake goes through the tunnel. Audio, video, and input
> negotiate a direct peer-to-peer path and never touch the tunnel provider.

You need a domain on Cloudflare (a free plan is fine) and `brew install cloudflared`.

## 1. Create a tunnel

```sh
cloudflared tunnel login
cloudflared tunnel create my-tunnel
```

That writes a credentials JSON into `~/.cloudflared/` and prints a tunnel UUID.
**Both are secrets** — never commit them.

## 2. Configure ingress

Create `~/.cloudflared/config.yml`:

```yaml
tunnel: <YOUR-TUNNEL-UUID>
credentials-file: /Users/<you>/.cloudflared/<YOUR-TUNNEL-UUID>.json
no-autoupdate: true

ingress:
  - hostname: remote.example.com
    service: http://127.0.0.1:9000
  # Everything else gets a hard 404 rather than reaching anything on this machine.
  - service: http_status:404
```

Use the literal `127.0.0.1`, not `localhost` — the latter can resolve to `::1`, where
nothing is listening.

Validate before going live:

```sh
cloudflared tunnel ingress validate
cloudflared tunnel ingress rule https://remote.example.com/
```

## 3. Route DNS and run

```sh
cloudflared tunnel route dns my-tunnel remote.example.com
cloudflared tunnel run my-tunnel
```

To keep it running across reboots, install it as a service
(`cloudflared service install`, or a LaunchAgent).

## 4. Verify before trusting it

Start the server, then from a browser with no session:

```sh
# Redirects to the login page — never serves the client.
curl -s -o /dev/null -w "%{http_code}\n" https://remote.example.com/

# With the server stopped: 502. Nothing is exposed when you are not using it.
```

Then work through the checklist in [SECURITY.md](SECURITY.md) — especially confirming
that a wrong password issues no cookie, and that the ICE candidate pair is not a
relay.

## Deliberately not a background service

Do not install the remote-desktop server itself as an always-on service. Run it when
you want it:

```sh
./dev.sh run -allow-input
```

The hostname returns 502 the rest of the time. An endpoint that can type into your
computer should exist only while you are using it — and that is a meaningful part of
the security posture, not an inconvenience to engineer away.

## Should you add an identity provider?

Yes, if this is anything more than personal use. Cloudflare Access puts a real login
(email OTP, Google, GitHub) in front of the hostname, so a request reaches your Mac
only after passing an identity check. That is strictly stronger than the built-in
password, which has no per-user accounts, no 2FA, and no audit log.

The built-in gate is the floor, not the ceiling.

## Networks that block direct UDP

Some managed and corporate networks block UDP or force a proxy, which prevents the
peer-to-peer path from forming. **There is no TURN relay fallback implemented**, so on
such a network this will not connect.

Before investing effort, test from the network you actually care about: open a
trickle-ICE candidate-gathering test and see which candidate types appear. `host` or
`srflx` means direct UDP works. If only `relay` appears, you would need TURN support —
ideally TURN-over-TLS on 443 — which is not built yet.
