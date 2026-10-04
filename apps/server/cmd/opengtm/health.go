package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"
)

func init() {
	register("health", "probe a running server's /healthz (container healthchecks)", runHealth)
}

// runHealth exists because the distroless image has no shell or curl.
func runHealth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8080/healthz", "health endpoint")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("health: " + resp.Status)
	}
	fmt.Println("ok")
	return nil
}
