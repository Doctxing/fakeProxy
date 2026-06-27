# fakeProxy

[中文](README_CN.md)

A Go reverse proxy that rewrites response content to route all browser traffic through local ports. Think of it as a transparent proxy that makes external sites appear as if they're hosted locally — useful for SSO ticket capture, authenticated scraping, and headless access from other devices.

Bind defaults to `0.0.0.0` so other machines on the LAN can use it directly. Each upstream origin gets its own random local port; an optional entry port 302-redirects to the main proxy URL.

## Quick start

### CLI

```bash
go build -o fakeproxy .
./fakeproxy https://example.com
# upstream:  https://example.com
# proxy:     http://0.0.0.0:54321
# entry:     http://0.0.0.0:54322
```

Flags:

| Flag | Default | Description |
|---|---|---|
| `-listen` | `127.0.0.1` | Local bind address |
| `-proxy` | (none) | Upstream HTTP proxy, e.g. `http://127.0.0.1:7890` |

### Library

```go
import core "github.com/doctxing/fakeProxy/core"

srv, _ := core.New(core.Config{})
defer srv.Close()

result, _ := srv.Start(context.Background(), "https://example.com")
fmt.Println(result.ProxyURL) // http://0.0.0.0:54321
fmt.Println(result.EntryURL) // http://0.0.0.0:54322
```

## How it works

```
Browser                  fakeProxy                 Upstream
   │                         │                         │
   │── GET :54321 ──────────►│                         │
   │                         │── GET example.com ─────►│
   │                         │◄──── 200 HTML ──────────│
   │                         │                         │
   │   Rewrite <a href="https://cdn.example.com/x">
   │        → <a href="http://192.168.3.139:54323/x">
   │                         │                         │
   │◄── 200 rewritten ───────│                         │
   │                         │                         │
   │── GET :54323/x ────────►│── GET cdn.example.com ─►│
   │◄── 200 ─────────────────│◄──── 200 ───────────────│
```

- Every upstream origin gets a random local port.
- All URLs in HTML, CSS, JSON, XML are rewritten to use the client's host + the correct local port.
- 302 Location headers are rewritten the same way.
- The entry port (`EntryURL`) just 302-redirects to `ProxyURL`, using whatever host the client accessed it with.

## Config

```go
type Config struct {
    BindAddr       string           // default "0.0.0.0"
    ProxyURL       *url.URL         // upstream HTTP proxy
    Transport      http.RoundTripper
    ResponseHook   ResponseHook     // inspect/block upstream responses
    RewriteHook    RewriteHook      // modify rewritten body
    RedirectRules  []RedirectRule   // pattern-based 302 rewriting
    MaxRewriteBody int64            // default 8 MiB
}
```

## Hooks

### ResponseHook

```go
type ResponseHook func(*Server, ResponseEvent) bool
```

Fires **after** the upstream response is received, **before** any URL rewriting. The event contains the original status code, headers, and body. Return `false` to block (client gets 403).

```go
ResponseHook: func(s *core.Server, ev core.ResponseEvent) bool {
    if ev.StatusCode == 302 {
        fmt.Println("redirect to:", ev.ResponseHeader.Get("Location"))
    }
    return true
},
```

### RewriteHook

```go
type RewriteHook func(*Server, RewriteEvent) []byte
```

Fires **after** the built-in URL rewriter. The event contains both the original body and the rewritten body. Return the final body to send to the client.

```go
RewriteHook: func(s *core.Server, ev core.RewriteEvent) []byte {
    return append(ev.RewrittenBody, []byte("<!-- injected -->")...)
},
```

### Hook timing

```
1. readBody(upstream response)
2. ResponseHook  ← raw status/headers/body, can block
3. rewriteRedirect (302 Location)
4. rewriteContent (HTML/CSS/JSON/XML URLs)
5. RewriteHook   ← original + rewritten body, can modify
6. write response to client
```

## Redirect rules

Pattern-match upstream 302 Location headers and replace them — no Go code needed.

```go
RedirectRules: []core.RedirectRule{
    {
        Pattern:     `^https?://trust\.hitsz\.edu\.cn\?.*ticket=([^&]+)`,
        Replacement: "http://myapp/callback?ticket=$1",
    },
},
```

Rules are checked in order; first match wins. Unmatched redirects are rewritten through the local proxy as usual.

## Entry port

`Start()` opens two listeners:

| URL | Purpose |
|---|---|
| `ProxyURL` | Proxies content, rewrites all URLs |
| `EntryURL` | 302 → `ProxyURL` using the client's host |

External devices only need `EntryURL`. Whatever IP they use to reach the entry port is carried through to the proxy URL in the 302 redirect, so resource URLs are rewritten with the correct host.

## Upstream proxy

Route fakeProxy's own outbound requests through another proxy:

```go
proxyURL, _ := url.Parse("http://127.0.0.1:7890")
srv, _ := core.New(core.Config{ProxyURL: proxyURL})
```

## Performance

Ryzen 7 9700X, Go 1.26, `-count=1`:

| Scenario | Time | Memory |
|---|---|---|
| Small HTML page (few links) | 32 µs | 20 KB |
| 64 KB HTML (500 links) | 611 µs | 449 KB |
| Entry redirect | 41 µs | 24 KB |
| Concurrent plain-text | 148 µs/op | 44 KB |

## License

MIT
