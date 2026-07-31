package stacktrail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBeaconDispatcherDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var redirectedRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				redirectedRequests.Add(1)
			}))
			defer target.Close()

			observedKey := make(chan string, 1)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observedKey <- r.Header.Get("X-API-Key")
				http.Redirect(w, r, target.URL, status)
			}))
			defer source.Close()

			dispatcher := newBeaconDispatcher(source.URL, "secret-canary")
			dispatcher.enqueue(beaconPayload{TraceID: "trace", SpanID: "span", JobName: "job"})

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := dispatcher.shutdown(ctx); err != nil {
				t.Fatalf("shutdown beacon dispatcher: %v", err)
			}
			select {
			case key := <-observedKey:
				if key != "secret-canary" {
					t.Fatalf("source received API key %q", key)
				}
			case <-ctx.Done():
				t.Fatal("source did not receive the start signal")
			}
			if got := redirectedRequests.Load(); got != 0 {
				t.Fatalf("redirect target received %d requests", got)
			}
		})
	}
}
func TestBeaconDispatcherDropsWhenQueueIsFull(t *testing.T) {
	dispatcher := &beaconDispatcher{queue: make(chan beaconPayload, 1)}
	dispatcher.queue <- beaconPayload{JobName: "first"}
	done := make(chan struct{})
	go func() {
		dispatcher.enqueue(beaconPayload{JobName: "dropped"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("enqueue blocked on a full queue")
	}
}

func TestBeaconDispatcherDoesNotRetryFailedResponse(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	dispatcher := newBeaconDispatcher(server.URL, "secret-canary")
	dispatcher.enqueue(beaconPayload{TraceID: "trace", SpanID: "span", JobName: "job"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := dispatcher.shutdown(ctx); err != nil {
		t.Fatalf("shutdown beacon dispatcher: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("sent %d requests", got)
	}
}
