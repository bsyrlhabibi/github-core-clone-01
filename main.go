package main

// Entry point: own the process lifecycle the harness expects. Bind an
// ephemeral port, register it with $MAKEFUN_ENV_URL, serve until the
// harness reports done, then exit 0. Registration treats 200 and 409 as
// success (a retried register may collide with the first attempt),
// fails fast on 400/405, and otherwise retries with backoff.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// contextWithTimeout is the per-call deadline for harness requests.
func contextWithTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

const (
	registerTimeout = 30 * time.Second
	pollTimeout     = 30 * time.Second
	initialBackoff  = 250 * time.Millisecond
	maxBackoff      = 2 * time.Second
)

var harnessBase string

var harnessClient = &http.Client{}

func fatalRegister(status int, body string) {
	fmt.Fprintf(os.Stderr, "forge entry: register failed with HTTP %d: %s\n", status, body)
	os.Exit(1)
}

// registerWithHarness posts our port until the harness accepts it.
func registerWithHarness(port int) {
	payload, _ := json.Marshal(map[string]interface{}{"port": port})
	backoff := initialBackoff
	for {
		ctx, cancel := contextWithTimeout(registerTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, harnessBase+"/register", bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, doErr := harnessClient.Do(req)
			if doErr == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				cancel()
				if resp.StatusCode == 200 || resp.StatusCode == 409 {
					return
				}
				if resp.StatusCode == 400 || resp.StatusCode == 405 {
					fatalRegister(resp.StatusCode, string(body))
				}
			} else {
				cancel()
			}
		} else {
			cancel()
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// waitForDone polls the harness until it reports the run is finished.
func waitForDone() {
	backoff := initialBackoff
	for {
		done := false
		ctx, cancel := contextWithTimeout(pollTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, harnessBase+"/done", nil)
		if err == nil {
			resp, doErr := harnessClient.Do(req)
			if doErr == nil {
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr == nil && resp.StatusCode == 200 {
					var parsed struct {
						Done bool `json:"done"`
					}
					if json.Unmarshal(body, &parsed) == nil && parsed.Done {
						done = true
					}
				}
			}
		}
		cancel()
		if done {
			return
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func main() {
	shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

	base := os.Getenv("MAKEFUN_ENV_URL")
	if base == "" {
		fmt.Fprintln(os.Stderr, "forge entry: MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	harnessBase = strings.TrimRight(base, "/")

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge entry: listen failed: %v\n", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{
		Handler: http.HandlerFunc(handleRequest),
	}
	go func() {
		_ = server.Serve(listener)
	}()

	fmt.Printf("forge entry: serving on port %d\n", port)
	registerWithHarness(port)
	fmt.Println("forge entry: registered with the harness; idling until it is done")
	waitForDone()
	fmt.Println("forge entry: harness is done; shutting down")
	_ = server.Close()
	os.Exit(0)
}
