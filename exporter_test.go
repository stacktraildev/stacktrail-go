package stacktrail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOTLPHTTPExporterDoesNotFollowRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()

	observedKey := make(chan string, 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedKey <- r.Header.Get("X-API-Key")
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := newClient(ctx, sdkConfig{
		apiKey:                 "secret-canary",
		collectorEndpoint:      source.URL,
		environment:            "development",
		serviceName:            "redirect-test",
		useHTTP:                true,
		allowInsecureTransport: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	client.startJob(context.Background(), "redirect-test").Success()
	_ = client.ForceFlush(ctx)
	_ = client.Shutdown(ctx)

	select {
	case key := <-observedKey:
		if key != "secret-canary" {
			t.Fatalf("source received API key %q", key)
		}
	case <-ctx.Done():
		t.Fatal("source did not receive telemetry")
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests", got)
	}
}
