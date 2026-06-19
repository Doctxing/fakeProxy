package fakeproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProxyRewritesHTMLAndLocation(t *testing.T) {
	t.Parallel()

	var seenUA atomic.Value
	targetTwo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer targetTwo.Close()

	targetOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.UserAgent())
		switch r.URL.Path {
		case "/jump":
			http.Redirect(w, r, targetTwo.URL+"/next?q=1", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<a href="` + targetTwo.URL + `/path?q=1">next</a>`))
		}
	}))
	defer targetOne.Close()

	var hookCount atomic.Int32
	srv := MustNew(Config{
		ResponseHook: func(s *Server, ev ResponseEvent) bool {
			hookCount.Add(1)
			if ev.StatusCode == 0 || ev.UpstreamURL == nil {
				t.Errorf("hook saw incomplete response event")
			}
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()

	localURL, err := srv.Start(ctx, targetOne.URL)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, localURL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "local-browser-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(body))
	}
	if strings.Contains(string(body), targetTwo.URL) {
		t.Fatalf("body still contains upstream URL: %s", string(body))
	}
	if !strings.Contains(string(body), "http://127.0.0.1:") {
		t.Fatalf("body does not contain local URL: %s", string(body))
	}
	if got := seenUA.Load(); got != "local-browser-test" {
		t.Fatalf("user agent = %v", got)
	}

	redirectClient := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	redirectResp, err := redirectClient.Get(localURL + "/jump")
	if err != nil {
		t.Fatal(err)
	}
	_ = redirectResp.Body.Close()
	location := redirectResp.Header.Get("Location")
	if redirectResp.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d", redirectResp.StatusCode)
	}
	if strings.Contains(location, targetTwo.URL) {
		t.Fatalf("location still contains upstream URL: %s", location)
	}
	if !strings.HasPrefix(location, "http://127.0.0.1:") || !strings.Contains(location, "/next?q=1") {
		t.Fatalf("unexpected rewritten location: %s", location)
	}
	if hookCount.Load() < 2 {
		t.Fatalf("hook called %d times, want at least 2", hookCount.Load())
	}
}

func TestHookCanBlock(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("secret"))
	}))
	defer target.Close()

	srv := MustNew(Config{
		ResponseHook: func(s *Server, ev ResponseEvent) bool {
			return false
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()

	localURL, err := srv.Start(ctx, target.URL)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestRewriteHookCanAdjustFinalBody(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<main>ok</main>`))
	}))
	defer target.Close()

	var sawHook atomic.Bool
	srv := MustNew(Config{
		RewriteHook: func(s *Server, ev RewriteEvent) []byte {
			sawHook.Store(true)
			if ev.Request == nil || ev.ContentType == "" {
				t.Errorf("rewrite hook saw incomplete event")
			}
			if !bytes.Contains(ev.OriginalBody, []byte(`<main>ok</main>`)) {
				t.Errorf("rewrite hook original body = %q", ev.OriginalBody)
			}
			return append(ev.RewrittenBody, []byte(`<!--hook-->`)...)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()

	localURL, err := srv.Start(ctx, target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !sawHook.Load() {
		t.Fatal("rewrite hook was not called")
	}
	if !strings.Contains(string(body), `<!--hook-->`) {
		t.Fatalf("rewrite hook output missing: %s", string(body))
	}
}

func TestHeadersAreAdaptedForUpstreamAndLocalClient(t *testing.T) {
	t.Parallel()

	var (
		seenReferer string
		seenOrigin  string
	)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenReferer = r.Header.Get("Referer")
		seenOrigin = r.Header.Get("Origin")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Security-Policy", "default-src https:")
		w.Header().Set("Strict-Transport-Security", "max-age=3600")
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	srv := MustNew(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()

	localURL, err := srv.Start(ctx, target.URL)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, localURL+"/path", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Referer", localURL+"/from?q=1")
	req.Header.Set("Origin", localURL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if !strings.HasPrefix(seenReferer, target.URL+"/from?q=1") {
		t.Fatalf("referer = %q, want upstream URL", seenReferer)
	}
	if seenOrigin != target.URL {
		t.Fatalf("origin = %q, want %q", seenOrigin, target.URL)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Fatalf("CSP leaked to local response: %q", got)
	}
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS leaked to local response: %q", got)
	}
}

func TestNewDomainRouteKeepsOriginOnly(t *testing.T) {
	t.Parallel()

	var (
		seenMu    sync.Mutex
		seenPaths []string
	)
	targetTwo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seenPaths = append(seenPaths, r.URL.Path)
		seenMu.Unlock()
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`body{color:black}`))
	}))
	defer targetTwo.Close()

	targetOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<script src="` + targetTwo.URL + `/first.js"></script><link href="` + targetTwo.URL + `/second.css">`))
	}))
	defer targetOne.Close()

	srv := MustNew(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()

	localURL, err := srv.Start(ctx, targetOne.URL)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	matches := regexp.MustCompile(`http://127\.0\.0\.1:\d+/second\.css`).FindStringSubmatch(string(body))
	if len(matches) != 1 {
		t.Fatalf("second resource was not rewritten to a local URL: %s", string(body))
	}

	resourceResp, err := http.Get(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	_ = resourceResp.Body.Close()
	if resourceResp.StatusCode != http.StatusOK {
		t.Fatalf("resource status = %d", resourceResp.StatusCode)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seenPaths) != 1 || seenPaths[0] != "/second.css" {
		t.Fatalf("upstream paths = %v, want [/second.css]", seenPaths)
	}
}

func TestURLPatternDoesNotRewriteJSLineComments(t *testing.T) {
	t.Parallel()

	srv := MustNew(Config{})
	defer srv.Close()
	req := &http.Request{Host: "127.0.0.1:18080"}

	js := `//JS原生方式-添加元素指定样式
addCss: function(that, cssJson){
	if(this.isNull(that)){
		return;
	}
	//$(that).css(cssJson);
	//var obj = document.getElementById("btnB");
	$(that)[0].style.cssText += ";"+cssJson;
},
//JS原生方式-添加元素指定样式
var real = "//cdn.example.com/app.js";
var realHTTP = "https://example.com/app.js";`

	out := srv.rewriteBody(req, "application/javascript", []byte(js))
	if string(out) != js {
		t.Fatalf("JavaScript body should not be rewritten:\n%s", string(out))
	}
}

func TestRewriteDoesNotCorruptEscapedSVGNamespace(t *testing.T) {
	t.Parallel()

	srv := MustNew(Config{})
	defer srv.Close()
	req := &http.Request{Host: "127.0.0.1:18080"}

	in := `jsl.dh(this.id,"\x3csvg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 24 24\">\x3c/svg>");`
	out := srv.rewriteBody(req, "application/javascript", []byte(in))

	if strings.Contains(string(out), "%5C\"") || strings.Contains(string(out), "127.0.0.1") {
		t.Fatalf("escaped SVG namespace was corrupted: %s", string(out))
	}
	if string(out) != in {
		t.Fatalf("namespace script changed:\n%s", string(out))
	}
}

func TestRewriteDoesNotTouchJavaScriptRegexLiterals(t *testing.T) {
	t.Parallel()

	srv := MustNew(Config{})
	defer srv.Close()
	req := &http.Request{Host: "127.0.0.1:18080"}

	in := `regex:{pattern:RegExp(/((?:^|[^$\w\xA0-\uFFFF."'\])\s]|\b(?:return|yield))\s*)/.source + /\//.source + "(?:" + /(?:\[(?:[^\]\\\r\n]|\\.)*\]|\\.|[^/\\\[\r\n])+\/[dgimyus]{0,7}/.source),lookbehind:!0}`
	out := srv.rewriteBody(req, "text/javascript", []byte(in))

	if string(out) != in {
		t.Fatalf("JavaScript regex literal was rewritten:\n%s", string(out))
	}
}

func TestRewriteHTMLDoesNotTouchInlineScriptButRewritesScriptSrc(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("console.log('ok')"))
	}))
	defer target.Close()

	srv := MustNew(Config{})
	defer srv.Close()
	req := &http.Request{Host: "127.0.0.1:18080"}

	inline := `var real = "https://cdn.example.com/app.js";`
	in := `<html><head><script src="` + target.URL + `/app.js"></script><script>` + inline + `</script></head></html>`
	out := string(srv.rewriteBody(req, "text/html", []byte(in)))

	if strings.Contains(out, target.URL) || !strings.Contains(out, `<script src="http://127.0.0.1:`) {
		t.Fatalf("script src was not rewritten: %s", out)
	}
	if !strings.Contains(out, inline) {
		t.Fatalf("inline script was changed: %s", out)
	}
}

func TestRewriteCSSURLStopsAtClosingParen(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer target.Close()

	srv := MustNew(Config{})
	defer srv.Close()
	req := &http.Request{Host: "127.0.0.1:18080"}

	in := `.a{background-image:url(` + target.URL + `/done.png)}@media (forced-colors:active){.b{background:url("` + target.URL + `/check.png")}}`
	out := srv.rewriteBody(req, "text/css", []byte(in))
	text := string(out)

	if strings.Contains(text, "%29") || strings.Contains(text, "%7D") {
		t.Fatalf("CSS delimiters were swallowed into URL: %s", text)
	}
	if !strings.Contains(text, ")}@media") {
		t.Fatalf("CSS structure was not preserved: %s", text)
	}
	if strings.Contains(text, target.URL) {
		t.Fatalf("CSS resource URL was not rewritten: %s", text)
	}
}

func TestRewriteHTMLSkipsSemanticAttributes(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer target.Close()

	srv := MustNew(Config{})
	defer srv.Close()
	req := &http.Request{Host: "127.0.0.1:18080"}

	in := `<html itemtype="http://schema.org/WebPage"><body><svg xmlns="http://www.w3.org/2000/svg"></svg><img src="` + target.URL + `/logo.png"></body></html>`
	out := srv.rewriteBody(req, "text/html", []byte(in))
	text := string(out)

	if !strings.Contains(text, `itemtype="http://schema.org/WebPage"`) {
		t.Fatalf("schema.org itemtype was changed: %s", text)
	}
	if !strings.Contains(text, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Fatalf("SVG namespace was changed: %s", text)
	}
	if strings.Contains(text, target.URL) || !strings.Contains(text, `src="http://127.0.0.1:`) {
		t.Fatalf("img src was not rewritten correctly: %s", text)
	}
}
