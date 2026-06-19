package examples_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	fakeproxy "github.com/doctxing/fakeProxy"
)

func BenchmarkProxyHTMLRewrite(b *testing.B) {
	targetAsset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`body{color:#111}`))
	}))
	defer targetAsset.Close()

	targetPage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><link rel="stylesheet" href="` + targetAsset.URL + `/app.css"></head><body><a href="` + targetAsset.URL + `/next">next</a></body></html>`))
	}))
	defer targetPage.Close()

	srv := fakeproxy.MustNew(fakeproxy.Config{})
	defer srv.Close()

	localURL, err := srv.Start(context.Background(), targetPage.URL)
	if err != nil {
		b.Fatal(err)
	}

	client := benchmarkClient()
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		resp, err := client.Get(localURL)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkProxyConcurrentRequests(b *testing.B) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	srv := fakeproxy.MustNew(fakeproxy.Config{})
	defer srv.Close()

	localURL, err := srv.Start(context.Background(), target.URL)
	if err != nil {
		b.Fatal(err)
	}

	client := benchmarkClient()
	b.ReportAllocs()
	b.ResetTimer()

	b.SetParallelism(2)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(localURL)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}

func benchmarkClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 128,
			MaxConnsPerHost:     64,
		},
	}
}
