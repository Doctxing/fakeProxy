# fakeProxy

[English](README.md)

用 Go 写的反向代理，把上游站点的响应内容中的 URL 全部改写成本地代理地址，让浏览器流量留在代理链路内。主要用于 SSO 票据捕获、需要认证的爬虫、其他设备以无头设备身份访问等场景。

默认绑定 `0.0.0.0`，局域网内其他设备可以直接访问。每个上游域名分配一个随机本地端口，另外提供了一个入口端口，访问自动 302 跳转到代理 URL。

## 快速开始

### 命令行

```bash
go build -o fakeproxy .
./fakeproxy https://example.com
# upstream:  https://example.com
# proxy:     http://0.0.0.0:54321
# entry:     http://0.0.0.0:54322
```

参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `127.0.0.1` | 本地绑定地址 |
| `-proxy` | 无 | 上游 HTTP 代理，如 `http://127.0.0.1:7890` |

### 作为库使用

```go
import core "github.com/doctxing/fakeProxy/core"

srv, _ := core.New(core.Config{})
defer srv.Close()

result, _ := srv.Start(context.Background(), "https://example.com")
fmt.Println(result.ProxyURL) // http://0.0.0.0:54321
fmt.Println(result.EntryURL) // http://0.0.0.0:54322
```

## 工作原理

```
浏览器                    fakeProxy                 上游站点
   │                         │                         │
   │── GET :54321 ──────────►│                         │
   │                         │── GET example.com ─────►│
   │                         │◄──── 200 HTML ──────────│
   │                         │                         │
   │   改写 <a href="https://cdn.example.com/x">
   │        → <a href="http://192.168.3.139:54323/x">
   │                         │                         │
   │◄── 200 改写后 ──────────│                         │
   │                         │                         │
   │── GET :54323/x ────────►│── GET cdn.example.com ─►│
   │◄── 200 ─────────────────│◄──── 200 ───────────────│
```

- 每个上游域名分配一个随机本地端口。
- HTML、CSS、JSON、XML 中的 URL 会被改写成客户端的 host + 对应端口。
- 302 Location 头同样改写。
- 入口端口（`EntryURL`）只做一件事：302 跳转到 `ProxyURL`，host 用客户端请求里的 `Host`。

每次请求根据自己的 `Host` 头独立改写，不会出现多设备互相覆盖的问题。

## 配置

```go
type Config struct {
    BindAddr       string           // 默认 "0.0.0.0"
    ProxyURL       *url.URL         // 上游 HTTP 代理
    Transport      http.RoundTripper
    ResponseHook   ResponseHook     // 拦截/观察上游响应
    RewriteHook    RewriteHook      // 修改改写后的 body
    RedirectRules  []RedirectRule   // 基于模式的重定向改写
    MaxRewriteBody int64            // 默认 8 MiB
}
```

## 钩子函数

### ResponseHook

```go
type ResponseHook func(*Server, ResponseEvent) bool
```

在上游响应到达**之后**、URL 改写**之前**触发。事件中包含原始状态码、Header 和 Body。返回 `false` 则拦截（客户端收到 403）。

```go
ResponseHook: func(s *core.Server, ev core.ResponseEvent) bool {
    if ev.StatusCode == 302 {
        fmt.Println("重定向到:", ev.ResponseHeader.Get("Location"))
    }
    return true
},
```

### RewriteHook

```go
type RewriteHook func(*Server, RewriteEvent) []byte
```

在内置 URL 改写**之后**触发。事件中同时包含原始 body 和改写后的 body。返回值是最终发给客户端的 body。

```go
RewriteHook: func(s *core.Server, ev core.RewriteEvent) []byte {
    return append(ev.RewrittenBody, []byte("<!-- 注入内容 -->")...)
},
```

### 触发时序

```
1. 读取上游响应体
2. ResponseHook  ← 原始状态码/Header/Body，可拦截
3. 改写 302 Location
4. 改写内容 URL（HTML/CSS/JSON/XML）
5. RewriteHook   ← 原始+改写后 body，可修改
6. 发送响应给客户端
```

## 重定向规则

用正则匹配上游的 302 Location 并替换，不需要写 Go 代码。

```go
RedirectRules: []core.RedirectRule{
    {
        Pattern:     `^https?://trust\.hitsz\.edu\.cn\?.*ticket=([^&]+)`,
        Replacement: "http://myapp/callback?ticket=$1",
    },
},
```

规则按顺序检查，首个匹配生效。未匹配的重定向走默认逻辑（改写为本地代理地址）。

## 入口端口

`Start()` 会打开两个监听端口：

| URL | 作用 |
|---|---|
| `ProxyURL` | 代理内容，改写所有 URL |
| `EntryURL` | 302 跳转到 ProxyURL，使用客户端的 host |

外部设备只需要知道 `EntryURL`。客户端用什么 IP 访问入口端口，302 跳转和后续资源改写都会沿用这个 IP，不会出现 `0.0.0.0` 的情况。

## 上游代理

让 fakeProxy 的出站请求也走代理：

```go
proxyURL, _ := url.Parse("http://127.0.0.1:7890")
srv, _ := core.New(core.Config{ProxyURL: proxyURL})
```

## 性能

Ryzen 7 9700X, Go 1.26, `-count=1`:

| 场景 | 耗时 | 内存 |
|---|---|---|
| 小 HTML 页面 | 32 µs | 20 KB |
| 64 KB HTML（500 个链接） | 611 µs | 449 KB |
| Entry 跳转 | 41 µs | 24 KB |
| 并发纯文本 | 148 µs/op | 44 KB |

## License

MIT
