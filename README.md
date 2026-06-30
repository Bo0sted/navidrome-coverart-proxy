# navidrome-coverart-proxy

A small, hardened proxy that lets Discord display your Navidrome cover
art without exposing Navidrome to the internet.

Navidrome clients that support Discord's RPC integration like Feishin do work without Navidrome being public, but at the cost of a placeholder image displaying on your profile instead of your music's cover art. This is because Discord uses a bot to fetch cover art from your server. Deploying this proxy publicly means users can keep their Navidrome instance private, while still being able to display cover art on their profile. 

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Feishin proxy activation](#feishin)
- [Configuration](#configuration)
- [Security](#security-testing)
  - [Summary](#summary)

## How it works

A client such as Feishin requests album metadata from the proxy. Navidrome would
normally fill that response with image URLs pointing at its own internal address.
This proxy rewrites those to point at its public hostname instead, leaving the
underlying share token untouched, and strips the backend's name and version from
the response.

The client then hands the rewritten image URL to Discord. Discord's bot fetches
it from the proxy, which quietly relays the image from Navidrome and returns the
bytes. Discord receives a working image from a public address and never sees, or
needs, anything about the real backend.

The image URLs the proxy emits use Navidrome's share-token form
(`/share/img/<token>`), which carries only an album reference, no account credentials. The proxy is the only host that can reach Navidrome to use that token, so the token is useless to anyone who cannot already reach the proxy's
backend.

The proxy only recognizes the specific endpoints a client needs for cover art: the metadata lookup, the cover-art image, and the share-image path. Each is matched against an allowlist before anything is forwarded. A request to any other path never reaches Navidrome; the proxy returns a 404 without contacting the backend at all. The allowlist is also method-aware, so even an approved path rejects the wrong HTTP verb. Feishin is the only client supported right now, with room for more to be added down the line. Please post an issue for that.

## Quick start

```yaml
services:
  navidrome-coverart-proxy:
    container_name: navidrome-coverart-proxy
    image: ghcr.io/bo0sted/navidrome-coverart-proxy:latest
    restart: unless-stopped
    read_only: true
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    environment:
      NAVIDROME_INTERNAL_URL: "http://container_name_recommended:container_port"
      STREAMING_CLIENT: "feishin"
      PUBLIC_URL: "https://public_domain.com"
      # CACHE_MINUTES: 60
      # LOG_LEVEL: "info"
    # Optionally expose the container to your LAN
    # ports:
    #   - "8080:8080"
    # Ensure network defined below is shared with Navidrome container
    networks:
      - shared_navidrome_network
    # Reverse Proxy - Traefik Labels
    # labels:
      # - "traefik.enable=true"
      # - "traefik.http.routers.insert_router_name_here.rule=Host(`insert.host.here`)"
      # - "traefik.http.routers.insert_router_name_here.entrypoints=insert_entrypoint_here"
      # - "traefik.http.routers.insert_router_name_here.tls=true"
      # - "traefik.http.services.insert_router_name_here.loadBalancer.server.port=8080"
networks:
  shared_navidrome_network:
    external: true
```

The proxy must share a Docker network with Navidrome so it can resolve the
backend by name. `NAVIDROME_INTERNAL_URL` should point at the backend over that
internal network and must not be a public address, keeping it private is the
entire point.

## Feishin
Steps to activate proxy in Feishin:
- Open the top left hamburger menu next to the search bar
- Click on your server
- In the newly opened menu, click "Manage servers"
- Click on your server once more to reveal the Edit button
- In the edit menu, locate the "Public URL" field
- Paste the public URL for your proxy
### Sidenote
Checking "Prefer public URL" will route _all_ music streaming through your proxy and break Feishin since this proxy was not designed for this use case. Leave that box unchecked and paste your real Navidrome server URL into the "URL" field.

Done! Feishin will now serve Discord your cover art through the _Public URL field_, and continue streaming your music through the _URL field_. 



## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `NAVIDROME_INTERNAL_URL` | Yes | — | Internal address of the backend, e.g. `http://navidrome:4533`. Must be reachable over the shared Docker network. |
| `STREAMING_CLIENT` | Yes | — | Which client's endpoint set to serve. Currently `feishin`. |
| `PUBLIC_URL` | Yes | — | The public base URL this proxy is reached at, e.g. `https://coverart.example.com`. Backend URLs are rewritten to this. |
| `CACHE_MINUTES` | No | `60` | In-memory cache lifetime for images. `0` disables caching. |
| `LOG_LEVEL` | No | `info` | `info` for failures and lifecycle only; `debug` adds per-request detail. |
| `LISTEN_PORT` | No | `8080` | Internal port the proxy listens on. If changed, update the `ports` mapping to match. |

## Security testing

The proxy's job is to let cover art through while ensuring none of the private
server details escape: its address, the software it runs, its version, your
login token, or your library.

### What the proxy protects

Each of these is enforced in code, not left to convention:

- **The backend never goes public.** Navidrome is reachable only over the shared
  internal Docker network. The proxy is the only host that can talk to it, so its
  address, ports, and the service itself never touch the internet, and the share
  tokens it emits are useless to anyone who can't already reach the backend.
- **A strict endpoint allowlist.** Only the three cover-art paths are recognized:
  the metadata lookup, the cover-art image, and the share-image path. Every other
  path gets a 404 the proxy generates itself, so it never reaches Navidrome, with
  no traversal, no pass-through fallback, and no route to the library or admin API.
- **Method-aware routing.** Each path accepts one method (metadata is POST-only,
  images are GET/HEAD). Wrong verbs are rejected before the backend is contacted.
- **Input validation on everything forwarded.** Only a fixed query-parameter set
  (`id`, `u`, `s`, `t`, `v`, `c`, `size`, `f`) is passed upstream; `id` must match
  a tight pattern (≤64 chars), `size` must be numeric, and share tokens are
  pattern-checked. Junk and injection-style input never reaches Navidrome.
- **Server fingerprint stripped.** The backend's name and version (`type`,
  `serverVersion`) are removed from every metadata response, JSON and XML, errors
  included.
- **Internal addresses rewritten, with a fail-safe.** Backend image links are
  rewritten to the public hostname; if any backend reference survives the rewrite,
  the proxy discards the body and returns a safe empty response.
- **No spoofable client data reaches the backend.** Forwarding headers
  (`X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Forwarded-Port`,
  `X-Real-Ip`, `Forwarded`) are stripped before the upstream request.
- **Image content-type enforced.** A response is only returned if the backend
  labeled it `image/*`; anything else becomes a 404.
- **Bounded resource use.** Image bodies (25 MiB), metadata bodies (8 KiB), and
  headers are capped, and every upstream call has a timeout.
- **Stateless, no credentials stored.** No accounts, no passwords, no persistent
  state beyond an in-memory image cache.

The container is also run locked down in the [quick start](#quick-start):
read-only root filesystem, all capabilities dropped, and `no-new-privileges` set.

## Verification

To confirm the above, each request below was run two ways, once straight to the
unprotected backend and once through the proxy. Every output shown is a real
response captured during testing.

Placeholders are in caps: `HOST` is whichever endpoint is being tested (the
backend directly, or the proxy), `BACKEND` is the private server address,
`PUBLIC` is the proxy's public hostname, `ID` is an album or cover ID, and
`AUTH` is the login parameters (`u`, `s`, `t`, `v`, `c`).

### Test 1 — Album info

Album info is normally filled with links pointing straight at the private
server. This is the largest leak risk. The endpoint expects a POST.

```
curl -X POST "http://HOST/rest/getAlbumInfo2.view?id=ID&AUTH&f=json"
```

Unprotected, the response names the software, gives the exact version and build,
and points every image link at the real internal address:

```json
{"subsonic-response":{"status":"ok","version":"1.16.1","type":"navidrome","serverVersion":"0.62.0 (1b46b977)","openSubsonic":true,"albumInfo":{
  "lastFmUrl":"https://www.last.fm/music/...",
  "smallImageUrl":"http://BACKEND/share/img/<token>?size=300",
  "mediumImageUrl":"http://BACKEND/share/img/<token>?size=600",
  "largeImageUrl":"http://BACKEND/share/img/<token>?size=1200"
}}}
```

Through the proxy, every image link points at the public address, the `type` and
`serverVersion` fields are gone, and the login token is never echoed back:

```json
{"subsonic-response":{"status":"ok","version":"1.16.1","openSubsonic":true,"albumInfo":{
  "lastFmUrl":"https://www.last.fm/music/...",
  "smallImageUrl":"https://PUBLIC/share/img/<token>?size=300",
  "mediumImageUrl":"https://PUBLIC/share/img/<token>?size=600",
  "largeImageUrl":"https://PUBLIC/share/img/<token>?size=1200"
}}}
```

The `<token>` is Navidrome's share token. It encodes only an album reference and
issuer, no account credentials, and can only be used by a host that can
already reach the backend.

### Test 2 — Cover art image headers

Every image response carries technical headers.

```
curl -D - -o /dev/null "http://HOST/rest/getCoverArt.view?id=ID&AUTH"
```

Unprotected, the response includes headers (`Last-Modified`,
`Permissions-Policy`, `Referrer-Policy`, `Vary`, `X-Content-Type-Options`,
`X-Frame-Options`, `Transfer-Encoding`, and more) that together help identify
the software even with no name attached.

Through the proxy, the response is reduced to a small neutral set:

```
HTTP/1.1 200 OK
Cache-Control: public, max-age=3600
Content-Length: 21090
Content-Type: image/webp
X-Cache: MISS
```

### Test 3 — Failed login

A request with a wrong token, to see what a rejection reveals. As with all
metadata requests, this is a POST.

```
curl -X POST "http://HOST/rest/getAlbumInfo2.view?id=ID&u=USER&t=wrong&s=wrong"
```

Unprotected, even a failed login fingerprints the server:

```json
{"subsonic-response":{"status":"failed","version":"1.16.1","type":"navidrome","serverVersion":"0.62.0 (1b46b977)","openSubsonic":true,"error":{"code":40,"message":"Wrong username or password"}}}
```

Through the proxy, the rejection carries nothing about the backend:

```json
{"subsonic-response":{"status":"failed","version":"1.16.1","openSubsonic":true,"error":{"code":40,"message":"Wrong username or password"}}}
```

### Test 4 — Missing data

A request for an album that does not exist, in case an error reveals details.

```
curl -X POST "http://HOST/rest/getAlbumInfo2.view?id=DOESNOTEXIST&AUTH"
```

Unprotected, a missing item still exposes the name and version:

```json
{"subsonic-response":{"status":"failed","version":"1.16.1","type":"navidrome","serverVersion":"0.62.0 (1b46b977)","openSubsonic":true,"error":{"code":70,"message":"data not found"}}}
```

Through the proxy:

```json
{"subsonic-response":{"status":"failed","version":"1.16.1","openSubsonic":true,"error":{"code":70,"message":"data not found"}}}
```

### Test 5 — A path that is not allowlisted

The proxy forwards only the cover-art endpoints. Any other path is refused
before the backend is ever contacted. Here the request uses valid credentials
and asks for the full artist list, an endpoint the proxy does not serve.

```
curl "http://HOST/rest/getArtists.view?AUTH&f=json"
```

Sent to the backend directly, the authenticated request returns the entire
artist list and sets a session cookie in the response:

```
HTTP/1.1 200 OK
Set-Cookie: nd-player-...; Path=/; HttpOnly; SameSite=Strict
{"subsonic-response":{"status":"ok","type":"navidrome","serverVersion":"0.62.0 (1b46b977)", ... full artist list ... }}
```

The same request through the proxy never reaches the backend. The path is not on
the allowlist, so the proxy returns a 404 on its own:

```
404
```

This is about exposure, not authentication: the backend behaves normally for an
authenticated client, but the proxy only ever forwards the handful of cover-art
endpoints, so everything else, including the library, stays out of reach of the
internet.

### Test 6 — Wrong method on an allowlisted path

The allowlist is method-aware. Each endpoint accepts a single method; the
metadata endpoint is POST-only. A GET to it is refused without contacting the
backend.

```
curl "http://PROXY/rest/getAlbumInfo2.view?id=ID&AUTH"
```

```
method not allowed
```

### Test 7 — Health check

A status check.

```
curl "http://PROXY/healthcheck"
```

```
Proxy: OK
Backend: Reachable
```

This confirms the proxy is running and the backend is reachable as a simple yes or
no, without naming or locating the backend.

### Summary

Sent to the backend directly, the responses give up its name, version, exact
build, internal address, a fingerprint, a session cookie,
and, for any endpoint a client asks for, the data behind it.

Through the proxy, the image links are rewritten to the public hostname, the
identifying fields are stripped, and only the cover-art endpoints are answered
at all. The cover art still reaches Discord as intended, while the backend stays
private.
