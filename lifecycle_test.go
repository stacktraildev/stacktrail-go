package stacktrail

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestJobBoundsCustomerControlledTelemetry(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	limits := sdktrace.NewSpanLimits()
	limits.AttributeValueLengthLimit = maxTelemetryValueRunes
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithRawSpanLimits(limits),
	)
	client := &client{tracer: provider.Tracer("test"), tracerProvider: provider}

	job := client.startJob(context.Background(), "   ")
	job.AddMetadata("   ", "ignored")
	job.AddMetadata(strings.Repeat("k", maxMetadataKeyRunes+1), strings.Repeat("v", maxTelemetryValueRunes+1))
	job.AddEvent("   ")
	job.AddEvent(strings.Repeat("e", maxTelemetryNameRunes+1))
	job.Fail(errors.New(strings.Repeat("x", maxTelemetryValueRunes+1)))

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans", len(spans))
	}
	span := spans[0]
	if span.Name != "unnamed-job" {
		t.Fatalf("job name is %q", span.Name)
	}
	if len([]rune(span.Status.Description)) != maxTelemetryValueRunes {
		t.Fatalf("status description has %d runes", len([]rune(span.Status.Description)))
	}
	var foundMetadata, foundCanonicalMessage, foundLegacyError bool
	for _, value := range span.Attributes {
		switch string(value.Key) {
		case "metadata." + strings.Repeat("k", maxMetadataKeyRunes):
			foundMetadata = len([]rune(value.Value.AsString())) == maxTelemetryValueRunes
		case "job.error.message":
			foundCanonicalMessage = len([]rune(value.Value.AsString())) == maxTelemetryValueRunes
		case "job.error":
			foundLegacyError = true
		case "metadata.":
			t.Fatal("blank metadata key was recorded")
		}
	}
	if !foundMetadata || !foundCanonicalMessage || foundLegacyError {
		t.Fatalf("bounded metadata=%t canonical_message=%t legacy_error=%t", foundMetadata, foundCanonicalMessage, foundLegacyError)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown provider: %v", err)
	}
}

type shutdownSignalProcessor struct {
	once   sync.Once
	called chan struct{}
}

func (*shutdownSignalProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (*shutdownSignalProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (*shutdownSignalProcessor) ForceFlush(context.Context) error                { return nil }
func (p *shutdownSignalProcessor) Shutdown(context.Context) error {
	p.once.Do(func() { close(p.called) })
	return nil
}

func TestClientShutdownDoesNotWaitForBeaconsBeforeProvider(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	beacons := newBeaconDispatcher(server.URL, "secret-canary")
	beacons.enqueue(beaconPayload{TraceID: "trace", SpanID: "span", JobName: "job"})
	processor := &shutdownSignalProcessor{called: make(chan struct{})}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
	client := &client{tracerProvider: provider, beacons: beacons}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- client.Shutdown(ctx) }()

	select {
	case <-requestStarted:
	case <-ctx.Done():
		t.Fatal("start signal request did not begin")
	}
	select {
	case <-processor.called:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("trace provider waited for the start signal to finish")
	}
	close(releaseRequest)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown client: %v", err)
	}
}
func TestJobCompletionIsIdempotentAndChildKeepsParent(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	client := &client{tracer: provider.Tracer("test"), tracerProvider: provider}

	parent := client.startJob(context.Background(), "parent")
	child := parent.StartChildJob("child")
	child.Success()
	parent.Success()
	parent.Fail(errors.New("ignored duplicate completion"))

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported %d spans", len(spans))
	}
	var parentSpan, childSpan tracetest.SpanStub
	for _, span := range spans {
		switch span.Name {
		case "parent":
			parentSpan = span
		case "child":
			childSpan = span
		}
	}
	if childSpan.Parent.SpanID() != parentSpan.SpanContext.SpanID() {
		t.Fatal("child does not reference the parent span")
	}
	if childSpan.SpanContext.TraceID() != parentSpan.SpanContext.TraceID() {
		t.Fatal("child and parent use different traces")
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown provider: %v", err)
	}
}
func TestPrivateProviderDoesNotReplaceGlobalProvider(t *testing.T) {
	globalProvider := otel.GetTracerProvider()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := newClient(ctx, sdkConfig{
		apiKey:                 "secret-canary",
		collectorEndpoint:      "http://127.0.0.1:4318",
		environment:            "development",
		serviceName:            "provider-test",
		useHTTP:                true,
		allowInsecureTransport: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if got := otel.GetTracerProvider(); got != globalProvider {
		t.Fatal("Stacktrail replaced the global OpenTelemetry provider")
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown client: %v", err)
	}
}

func TestPackageLifecycleRejectsDuplicateInitAndAllowsReinit(t *testing.T) {
	_ = Shutdown(context.Background())
	t.Cleanup(func() { _ = Shutdown(context.Background()) })
	t.Setenv(envAPIKey, "secret-canary")
	t.Setenv(envEnvironment, "development")
	t.Setenv(envTransport, "http")
	t.Setenv(envCollectorEndpoint, "http://127.0.0.1:4318")
	t.Setenv(envSecure, "false")

	if err := Init(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := Init(context.Background()); err == nil {
		t.Fatal("duplicate initialization succeeded")
	}
	if err := Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := Init(context.Background()); err != nil {
		t.Fatalf("reinitialize: %v", err)
	}
}
func TestConcurrentJobMutationAndCompletion(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	client := &client{tracer: provider.Tracer("test"), tracerProvider: provider}
	job := client.startJob(context.Background(), "concurrent-job")

	var workers sync.WaitGroup
	for index := range 64 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			job.AddMetadata("worker", index)
			job.AddEvent("worker-finished")
			job.Success()
		}()
	}
	workers.Wait()
	if spans := exporter.GetSpans(); len(spans) != 1 {
		t.Fatalf("exported %d spans", len(spans))
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown provider: %v", err)
	}
}
