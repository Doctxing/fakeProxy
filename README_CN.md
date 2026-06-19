# fakeProxy

[English](README.md)

fakeProxy 是一个用 Go 写的小型代理工具。通过对页面里的资源改写成本地代理地址，让浏览器访问过程尽量留在本地代理链路里。从而让其他设备以无头设备的身份访问认证等等较为简单的站点，实际测试中也发现它对一些复杂站点的兼容性还不错。

理论来源：对于较为简单的网站，测试交付的过程大概率不会依赖其源域名，就是说内部的路径大概率都是相对路径的，这种假代理访问也并不会对各个站点造成什么负面影响，大部分站点也没有意识进行面向此流程的反制，目前的替换规则不包含`javascript`；如果目前的替换规则不满足需求，我提供了一个钩子函数供调用。

## 快速调用说明

使用 fakeProxy 只需要三步：

1. 用 `fakeproxy.New` 创建 `Server`。
2. 调用 `Start` 传入上游 URL。
3. 打开返回的本地 URL。

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

使用上游 HTTP 代理：

```go
proxyURL, _ := url.Parse("http://127.0.0.1:7890")

srv, err := fakeproxy.New(fakeproxy.Config{
	BindAddr: "127.0.0.1",
	ProxyURL: proxyURL,
})
```

观察上游响应：

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

修改内置改写后的响应体：

```go
srv, err := fakeproxy.New(fakeproxy.Config{
	RewriteHook: func(s *fakeproxy.Server, ev fakeproxy.RewriteEvent) []byte {
		return append(ev.RewrittenBody, []byte("\n<!-- modified -->")...)
	},
})
```

需要记住的行为：

- `Start` 返回的是本地访问 URL。
- 每个上游 origin 会分配一个随机本地端口。
- `ResponseHook` 在响应改写前执行。
- `RewriteHook` 在内置 body 改写后执行。
- `ProxyURL` 只影响 fakeProxy 到上游站点的出站请求。
