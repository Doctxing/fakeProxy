package examples_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fakeproxy "github.com/doctxing/fakeProxy"
)

func TestProxyRewritesHTMLAndLocation(t *testing.T) {
	t.Parallel()

	targetTwo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer targetTwo.Close()

	targetOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := fakeproxy.MustNew(fakeproxy.Config{
		ResponseHook: func(s *fakeproxy.Server, ev fakeproxy.ResponseEvent) bool {
			hookCount.Add(1)
			if ev.Request == nil || ev.LocalPort == 0 || ev.UpstreamURL == nil || ev.StatusCode == 0 {
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

	resp, err := http.Get(localURL)
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

func TestResponseHookCanBlock(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("secret"))
	}))
	defer target.Close()

	srv := fakeproxy.MustNew(fakeproxy.Config{
		ResponseHook: func(s *fakeproxy.Server, ev fakeproxy.ResponseEvent) bool {
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
	srv := fakeproxy.MustNew(fakeproxy.Config{
		RewriteHook: func(s *fakeproxy.Server, ev fakeproxy.RewriteEvent) []byte {
			sawHook.Store(true)
			if ev.Request == nil || ev.LocalPort == 0 || ev.ContentType == "" {
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
