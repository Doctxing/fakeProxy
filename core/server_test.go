package core_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/doctxing/fakeProxy/core"
)

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func TestBasicProxy(t *testing.T) {
	t.Parallel()

	// Two upstream targets: one with HTML, one with JSON.
	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer target2.Close()

	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jump" {
			http.Redirect(w, r, target2.URL+"/next?q=1", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<a href="` + target2.URL + `/path?q=1">next</a>`))
	}))
	defer target1.Close()

	var hookCalls atomic.Int32
	srv := core.MustNew(core.Config{
		ResponseHook: func(s *core.Server, ev core.ResponseEvent) bool {
			hookCalls.Add(1)
			return true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer srv.Close()

	result, err := srv.Start(ctx, target1.URL)
	if err != nil {
		t.Fatal(err)
	}
	if result.EntryURL == "" {
		t.Fatal("expected entry URL")
	}

	// --- HTML body rewriting ---
	resp, err := http.Get(result.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(string(body), target2.URL) {
		t.Fatal("body leaked upstream URL")
	}
	if !strings.Contains(string(body), "http://0.0.0.0:") {
		t.Fatal("body missing local URL")
	}

	// --- Redirect rewriting ---
	redir, err := noRedirectClient().Get(result.ProxyURL + "/jump")
	if err != nil {
		t.Fatal(err)
	}
	redir.Body.Close()

	loc := redir.Header.Get("Location")
	if redir.StatusCode != http.StatusFound {
		t.Fatalf("redirect status %d", redir.StatusCode)
	}
	if strings.Contains(loc, target2.URL) {
		t.Fatal("location leaked upstream URL")
	}
	if !strings.HasPrefix(loc, "http://0.0.0.0:") || !strings.Contains(loc, "/next?q=1") {
		t.Fatalf("bad rewritten location: %s", loc)
	}
	if hookCalls.Load() < 2 {
		t.Fatalf("hook called %d times", hookCalls.Load())
	}
}

func TestResponseHook(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{
		ResponseHook: func(s *core.Server, ev core.ResponseEvent) bool { return false },
	})
	defer srv.Close()

	result, _ := srv.Start(context.Background(), upstream.URL)
	resp, err := http.Get(result.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestRewriteHook(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<main>ok</main>`))
	}))
	defer upstream.Close()

	var called atomic.Bool
	srv := core.MustNew(core.Config{
		RewriteHook: func(s *core.Server, ev core.RewriteEvent) []byte {
			called.Store(true)
			if !bytes.Contains(ev.OriginalBody, []byte("<main>ok</main>")) {
				t.Error("original body mismatch")
			}
			return append(ev.RewrittenBody, []byte("<!--hook-->")...)
		},
	})
	defer srv.Close()

	result, _ := srv.Start(context.Background(), upstream.URL)
	resp, _ := http.Get(result.ProxyURL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !called.Load() {
		t.Fatal("rewrite hook not called")
	}
	if !strings.Contains(string(body), "<!--hook-->") {
		t.Fatal("hook output missing")
	}
}

func TestRedirectRule(t *testing.T) {
	t.Parallel()

	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer redirectTarget.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/landing", http.StatusFound)
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{
		RedirectRules: []core.RedirectRule{{
			Pattern:     `^` + regexp.QuoteMeta(redirectTarget.URL) + `.*`,
			Replacement: "http://example.com/custom",
		}},
	})
	defer srv.Close()

	result, _ := srv.Start(context.Background(), upstream.URL)
	resp, err := noRedirectClient().Get(result.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); loc != "http://example.com/custom" {
		t.Fatalf("rule didn't fire, got: %s", loc)
	}
}

func TestEntryRedirect(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{})
	defer srv.Close()

	result, _ := srv.Start(context.Background(), upstream.URL+"/hello?x=1")

	resp, err := noRedirectClient().Get(result.EntryURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("entry status %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/hello?x=1") {
		t.Fatalf("entry redirect lost path/query: %s", loc)
	}
}

func TestShutdown(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{})
	result, _ := srv.Start(context.Background(), upstream.URL)

	// Send a request to confirm it's alive.
	resp, err := http.Get(result.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Shutdown with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// After shutdown, requests should fail.
	if _, err := http.Get(result.ProxyURL); err == nil {
		t.Fatal("expected connection refused after shutdown")
	}
}

func TestMultipleOrigins(t *testing.T) {
	t.Parallel()

	var originB *httptest.Server

	originA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<a href="` + originB.URL + `/b">to B</a>`))
	}))
	defer originA.Close()

	originB = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("B"))
	}))
	defer originB.Close()

	srv := core.MustNew(core.Config{})
	defer srv.Close()

	result, _ := srv.Start(context.Background(), originA.URL)

	resp, _ := http.Get(result.ProxyURL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Both originA's URL (rewritten) and originB's URL (rewritten) should appear.
	if strings.Contains(string(body), originA.URL) {
		t.Fatal("originA URL leaked")
	}
	if strings.Contains(string(body), originB.URL) {
		t.Fatal("originB URL leaked")
	}
	if !strings.Contains(string(body), "http://0.0.0.0:") {
		t.Fatal("no local URLs in rewritten body")
	}
}
