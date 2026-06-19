package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	fakeproxy "github.com/doctxing/fakeProxy"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := fakeproxy.New(fakeproxy.Config{
		BindAddr: "127.0.0.1",
		ResponseHook: func(s *fakeproxy.Server, ev fakeproxy.ResponseEvent) bool {
			if ev.StatusCode != http.StatusFound {
				return true
			}

			location := ev.ResponseHeader.Get("Location")
			u, err := url.Parse(location)
			if err != nil || !strings.EqualFold(u.Hostname(), "trust.hitsz.edu.cn") {
				return true
			}

			ticket, ok := ticketValue(u)
			if !ok {
				return true
			}

			fmt.Println("ticket:", ticket)
			go func() {
				cancel()
				_ = s.Close()
			}()
			return false
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	localURL, err := srv.Start(ctx, "https://ids-hit-edu-cn-s.hitsz.edu.cn")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("open:", localURL)
	<-ctx.Done()
}

func ticketValue(u *url.URL) (string, bool) {
	values, ok := u.Query()["ticket"]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}
