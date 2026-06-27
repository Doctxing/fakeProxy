package core_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	core "github.com/doctxing/fakeProxy/core"
)

func benchClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			MaxConnsPerHost:     128,
		},
	}
}

func BenchmarkProxy(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><link rel="stylesheet" href="/a.css"><a href="/next">next</a></html>`))
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{})
	defer srv.Close()
	result, _ := srv.Start(context.Background(), upstream.URL)
	client := benchClient()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, _ := client.Get(result.ProxyURL)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkProxyParallel(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{})
	defer srv.Close()
	result, _ := srv.Start(context.Background(), upstream.URL)
	client := benchClient()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, _ := client.Get(result.ProxyURL)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkProxyLargeHTML(b *testing.B) {
	// ~64 KB of HTML with many internal links.
	page := "<html><body>"
	for i := 0; i < 500; i++ {
		page += `<a href="/page/` + string(rune('0'+i%10)) + `">link</a>`
	}
	page += "</body></html>"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{})
	defer srv.Close()
	result, _ := srv.Start(context.Background(), upstream.URL)
	client := benchClient()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, _ := client.Get(result.ProxyURL)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkEntryRedirect(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	srv := core.MustNew(core.Config{})
	defer srv.Close()
	result, _ := srv.Start(context.Background(), upstream.URL)
	client := benchClient()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, _ := client.Get(result.EntryURL)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
