# fakeProxy

[中文](README_CN.md)

fakeProxy is a small proxy tool written in Go. It rewrites page resources to local proxy addresses, keeping browser traffic inside the local proxy flow as much as possible. This makes it easier for other devices to access relatively simple authentication sites as if they were the headless device. In actual testing, it has also shown decent compatibility with some more complex sites.

The idea behind it is that for relatively simple websites, the tested delivery flow often does not strongly depend on the original source domain. In other words, internal paths are very likely to be relative paths. This kind of fake proxy access usually does not have a negative impact on those sites, and most sites do not intentionally defend against this workflow. The current replacement rules do not include `javascript`; if the current rewrite rules are not enough, a hook function is provided for custom handling.

## Quick Usage Guide

Use fakeProxy in three steps:

1. Create a `Server` with `fakeproxy.New`.
2. Call `Start` with the upstream URL.
3. Open the returned local URL.

```go
package main

import (
	"context"
	"fmt"
	"log"

	fakeproxy "github.com/doctxing/fakeProxy"
)

func main() {
	srv, err := fakeproxy.New(fakeproxy.Config{
		BindAddr: "127.0.0.1",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	localURL, err := srv.Start(context.Background(), "https://example.com")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(localURL)
}
```

Use an upstream HTTP proxy:

```go
proxyURL, _ := url.Parse("http://127.0.0.1:7890")

srv, err := fakeproxy.New(fakeproxy.Config{
	BindAddr: "127.0.0.1",
	ProxyURL: proxyURL,
})
```

Watch upstream responses:

```go
srv, err := fakeproxy.New(fakeproxy.Config{
	ResponseHook: func(s *fakeproxy.Server, ev fakeproxy.ResponseEvent) bool {
		if ev.StatusCode == 302 {
			fmt.Println(ev.ResponseHeader.Get("Location"))
		}
		return true
	},
})
```

Modify the rewritten body:

```go
srv, err := fakeproxy.New(fakeproxy.Config{
	RewriteHook: func(s *fakeproxy.Server, ev fakeproxy.RewriteEvent) []byte {
		return append(ev.RewrittenBody, []byte("\n<!-- modified -->")...)
	},
})
```

Important behavior:

- `Start` returns a local URL.
- Each upstream origin gets one random local port.
- `ResponseHook` runs before response rewriting.
- `RewriteHook` runs after built-in body rewriting.
- `ProxyURL` only affects outbound requests from fakeProxy to the upstream site.
