package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const healthcheckTimeout = 4 * time.Second

// healthcheck probes the already-running daemon. It deliberately opens no
// controller state, so the scratch-image health check needs neither the
// passphrase nor access to the database.
func healthcheck(listen string) error {
	target, err := healthcheckURL(listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: build request: %w", err)
	}
	// Never send an internal liveness probe through an ambient HTTP proxy.
	client := &http.Client{Transport: &http.Transport{}, Timeout: healthcheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3))
	if err != nil {
		return fmt.Errorf("healthcheck: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		return fmt.Errorf("healthcheck: unexpected response: status=%d body=%q",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func healthcheckURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("healthcheck: invalid listen address %q: %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
		Path:   "/healthz",
	}).String(), nil
}
