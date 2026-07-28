# Security model

**Read this before you expose anything.**

This tool injects keystrokes and mouse clicks into a Mac. Anything that can reach the
input channel can open Terminal and type into it. That makes it **structurally
identical to malware** and **remote code execution by design**. The safeguards below
are not polish — they are the reason it is safe to run at all.

## The invariants

1. **Loopback bind.** The server binds `127.0.0.1` by default and should stay there.
   Remote access goes through a tunnel that terminates locally. A wildcard bind
   (`-addr 0.0.0.0:...`) exposes an input-injection endpoint to your whole LAN; use it
   briefly on a trusted network for testing, then stop.

2. **Everything is gated.** The client page and the WebSocket upgrade both require
   authentication. There is no unauthenticated surface except the login page itself.

3. **View-only is the default.** Without `-allow-input`, the input data channel is
   never created — the browser has nothing to send control to. Control is opt-in per
   run, not a setting someone can flip remotely.

4. **The helper is the permission boundary.** One Swift binary holds both macOS
   grants (Screen Recording, Accessibility). It accepts commands **only** on stdin
   from the local Go process — never from a socket, never from the network.

5. **Everything from the browser is untrusted.** `translateInput` validates every
   field of every message — coordinate ranges, button numbers, key-code character
   sets, message size — before anything is written to the helper. A hostile client
   cannot smuggle arbitrary bytes into the injector. This is the trust boundary, and
   it has unit tests.

6. **No loopback exemption in auth.** The host process authenticates with a bearer
   token exactly like a browser authenticates with a cookie. "This connection looks
   local, let it through" is an extra branch that can only ever become a bug.

## How authentication works

Two ways in, one check:

- **Browsers** POST the password once and receive an HMAC-signed session cookie
  (`HttpOnly`, `SameSite=Lax`, `Secure` when the request arrived over HTTPS, 24-hour
  expiry). The browser then sends it automatically on the same-origin WebSocket
  upgrade — which is *why* a cookie is used rather than a token, since JavaScript
  cannot set headers on a WebSocket handshake.
- **The local host process** sends `Authorization: Bearer <token>`.

Sessions are stateless: the cookie is `<expiry>.<hmac>`, verified against a per-install
secret. Changing the password rotates that secret, which immediately invalidates every
existing session — including a stolen one.

### Secrets

Stored in `~/.config/play/config.json`, mode `0600`:

| Field | Purpose |
|---|---|
| `password_hash` | bcrypt. The plaintext is printed once at creation and never stored. |
| `token` | Bearer token for the local host process. |
| `cookie_secret` | HMAC key for session cookies. Rotated on password change. |

A generated password carries 96 bits of entropy. If you set your own, make it long —
this is an internet-facing gate on a tool that can type into your computer.

### Brute-force resistance

Two layers, because per-IP throttling alone is defeated by anyone with a proxy pool:

- **Per client IP:** 10 failures triggers a 1-minute lockout. Behind a tunnel every
  request appears to come from `127.0.0.1`, so `CF-Connecting-IP` is used when present
  to keep one attacker from locking out everyone.
- **Globally:** past 30 failed attempts per minute across all clients, every login
  attempt is *slowed* to 2 seconds — and verified one at a time, so that ceiling holds
  no matter how many connections or source addresses an attacker opens. Far above any
  human fumbling a password, far below what makes dictionary attacks practical.

The global layer slows attempts rather than refusing them, and that distinction is the
whole design. Your login goes through the same door as the attacker's, so anything the
throttle refuses outright is something an attacker can refuse *on your behalf* — 30
POSTs a minute from a handful of addresses would buy them a permanent lockout of your
own machine. A correct password is never rejected by the global cap, only delayed.

## What this does NOT protect against

Be clear-eyed about the gaps:

- **A single shared password.** No per-user accounts, no 2FA, no audit log, no
  revocation beyond changing the one password. For anything beyond personal use, put a
  real identity provider in front (Cloudflare Access, an OAuth proxy).
- **A compromised browser or host machine.** If either endpoint is owned, so is the
  session.
- **Physical access to the Mac.** Unchanged by anything here.
- **Your tunnel provider.** Signaling passes through it in plaintext at their edge.
  Media does not — it is DTLS-encrypted and peer-to-peer — but SDP does.
- **Denial of service.** Nothing here stops someone flooding your tunnel. The global
  throttle is deliberately built *not* to make that worse — it delays logins instead of
  refusing them, so a sustained attack makes signing in slow rather than impossible —
  but slow is still a cost you pay and they don't.

## Before you expose it: attack your own setup

From a browser with no session, confirm every one of these:

| Check | Expected |
|---|---|
| `GET /` and `/index.html` | redirect to the login page |
| WebSocket upgrade to `/ws` | rejected (401 locally; a redirect through some tunnels) |
| A wrong password | **no cookie issued** |
| The server stopped | your hostname returns 502 — nothing exposed when not running |
| Direct connection to port 9000 from another machine | refused (loopback bind) |

Then check `getStats()` in the browser: the nominated ICE candidate pair should not be
`relay`, confirming media is peer-to-peer rather than passing through infrastructure.

Deliberately **not** running this as a background service is part of the model: the
endpoint exists only while you are using it.

## Reporting

This is a personal learning project with no security guarantees and no formal advisory
process. If you find something, open an issue. Do not run it in an environment where a
compromise would matter.
