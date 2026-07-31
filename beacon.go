package stacktrail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	beaconQueueCapacity  = 256
	beaconWorkerCount    = 4
	beaconRequestTimeout = time.Second
)

type beaconPayload struct {
	TraceID     string `json:"trace_id"`
	SpanID      string `json:"span_id"`
	JobName     string `json:"job_name"`
	StartedAt   string `json:"started_at"`
	Environment string `json:"environment"`
}

type beaconDispatcher struct {
	endpoint string
	apiKey   string
	client   *http.Client
	queue    chan beaconPayload
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.RWMutex
	closed   bool
}

func newBeaconDispatcher(endpoint, apiKey string) *beaconDispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	dispatcher := &beaconDispatcher{
		endpoint: endpoint,
		apiKey:   apiKey,
		client:   newNoRedirectHTTPClient(beaconRequestTimeout),
		queue:    make(chan beaconPayload, beaconQueueCapacity),
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}

	var workers sync.WaitGroup
	workers.Add(beaconWorkerCount)
	for range beaconWorkerCount {
		go func() {
			defer workers.Done()
			dispatcher.run()
		}()
	}
	go func() {
		workers.Wait()
		close(dispatcher.done)
	}()
	return dispatcher
}

func beaconEndpoint(config sdkConfig) string {
	if !config.hostedHTTP {
		return ""
	}
	return "https://" + defaultHostedEndpoint + "/v1/job-started"
}

func (d *beaconDispatcher) enqueue(payload beaconPayload) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	select {
	case d.queue <- payload:
	default:
	}
}

func (d *beaconDispatcher) shutdown(ctx context.Context) error {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()

	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		d.cancel()
		<-d.done
		return ctx.Err()
	}
}

func (d *beaconDispatcher) run() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case payload, ok := <-d.queue:
			if !ok {
				return
			}
			d.send(payload)
		}
	}
}

func (d *beaconDispatcher) send(payload beaconPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, beaconRequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", d.apiKey)

	response, err := d.client.Do(request)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return
	}
}
