package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	core "github.com/doctxing/fakeProxy/core"
)

func main() {
	listen := flag.String("listen", "127.0.0.1", "local bind address")
	proxyRaw := flag.String("proxy", "", "upstream proxy URL, for example http://user:pass@host:port")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: fakeproxy [flags] <http-or-https-url>\n")
		flag.PrintDefaults()
		os.Exit(2)
	}

	var proxyURL *url.URL
	if *proxyRaw != "" {
		parsed, err := url.Parse(*proxyRaw)
		if err != nil {
			log.Fatalf("parse proxy URL: %v", err)
		}
		proxyURL = parsed
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := core.New(core.Config{
		BindAddr: *listen,
		ProxyURL: proxyURL,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	localURL, err := srv.Start(ctx, flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("upstream:  %s\n", flag.Arg(0))
	fmt.Printf("proxy:     %s\n", localURL.ProxyURL)
	if localURL.EntryURL != "" {
		fmt.Printf("entry:     %s\n", localURL.EntryURL)
	}
	<-ctx.Done()
}
