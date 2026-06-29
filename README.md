# navidrome-coverart-proxy

A small, hardened proxy that lets Discord display your Navidrome cover
art without exposing Navidrome to the internet.

Navidrome clients that support Discord's RPC integration like Feishin do work without Navidrome being public, but at the cost of a placeholder image displaying on your profile instead of your music's cover art. This is because Discord uses a bot to fetch cover art from your server. Deploying this proxy publicly means users can keep their Navidrome instance private, while still being able to display cover art on their profile. 

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Security testing](#security-testing)
  - [Test 1 — Album info](#test-1--album-info)
  - [Test 2 — Cover art image headers](#test-2--cover-art-image-headers)
  - [Test 3 — Failed login](#test-3--failed-login)
  - [Test 4 — Missing data](#test-4--missing-data)
  - [Test 5 — Forbidden endpoint](#test-5--forbidden-endpoint)
  - [Test 6 — Health check](#test-6--health-check)
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
internal network and must not be a public address — keeping it private is the
entire point.

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

To confirm this, each request below was run two ways, once straight to the
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
