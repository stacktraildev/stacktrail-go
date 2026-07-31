package stacktrail

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	envAPIKey            = "STACKTRAIL_API_KEY" // #nosec G101 -- environment variable name, not a credential
	envServiceName       = "STACKTRAIL_SERVICE_NAME"
	envTransport         = "STACKTRAIL_TRANSPORT"
	envCollectorEndpoint = "STACKTRAIL_COLLECTOR_ENDPOINT"
	envEnvironment       = "STACKTRAIL_ENV"
	envSecure            = "STACKTRAIL_SECURE"

	defaultHostedEndpoint = "api.stacktrail.com:443"
)

var defaultClient struct {
	sync.RWMutex
	client *client
}

type client struct {
	tracer         trace.Tracer
	tracerProvider *sdktrace.TracerProvider
	environment    string
	beacons        *beaconDispatcher
}

type sdkConfig struct {
	apiKey                 string
	collectorEndpoint      string
	environment            string
	serviceName            string
	useHTTP                bool
	allowInsecureTransport bool
	hostedHTTP             bool
}

// LoadDotEnv loads one explicit .env file without overwriting values already
// supplied by the process environment. It never searches parent directories.
func LoadDotEnv(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New(".env path is required")
	}
	return godotenv.Load(path)
}

// Init configures Stacktrail's package-level client from the process
// environment. After a successful call, use StartJob and Shutdown directly.
func Init(ctx context.Context) error {
	config, err := configFromEnv()
	if err != nil {
		return err
	}
	newClient, err := newClient(ctx, config)
	if err != nil {
		return err
	}
	if err := installDefaultClient(newClient); err != nil {
		_ = newClient.Shutdown(ctx)
		return err
	}
	return nil
}

func configFromEnv() (sdkConfig, error) {
	config := sdkConfig{
		apiKey:            strings.TrimSpace(os.Getenv(envAPIKey)),
		environment:       strings.TrimSpace(os.Getenv(envEnvironment)),
		serviceName:       strings.TrimSpace(os.Getenv(envServiceName)),
		collectorEndpoint: strings.TrimSpace(os.Getenv(envCollectorEndpoint)),
		useHTTP:           true,
	}

	if transport, ok := os.LookupEnv(envTransport); ok && strings.TrimSpace(transport) != "" {
		switch strings.ToLower(strings.TrimSpace(transport)) {
		case "http":
			config.useHTTP = true
		case "grpc":
			config.useHTTP = false
		default:
			return sdkConfig{}, fmt.Errorf("%s must be http or grpc", envTransport)
		}
	}

	secure, err := secureEnv()
	if err != nil {
		return sdkConfig{}, err
	}
	config.allowInsecureTransport = !secure

	if !config.useHTTP {
		if config.collectorEndpoint == "" {
			config.collectorEndpoint = "localhost:4317"
		}
		if err := validateGRPCEndpoint(config.collectorEndpoint); err != nil {
			return sdkConfig{}, err
		}
		return config, nil
	}

	if config.collectorEndpoint == "" {
		if !secure {
			return sdkConfig{}, fmt.Errorf("%s=false requires an explicit custom HTTP endpoint", envSecure)
		}
		config.collectorEndpoint = defaultHostedEndpoint
		config.hostedHTTP = true
		return config, nil
	}

	if err := validateHTTPEndpoint(config.collectorEndpoint, secure); err != nil {
		return sdkConfig{}, err
	}
	if !secure && isHostedHTTPEndpoint(config.collectorEndpoint) {
		return sdkConfig{}, fmt.Errorf("%s=false requires a custom non-hosted HTTP endpoint", envSecure)
	}
	config.hostedHTTP = secure && isDefaultHostedHTTPEndpoint(config.collectorEndpoint)
	return config, nil
}

func secureEnv() (bool, error) {
	value, ok := os.LookupEnv(envSecure)
	if !ok || strings.TrimSpace(value) == "" {
		return true, nil
	}
	secure, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", envSecure, err)
	}
	return secure, nil
}

func validateHTTPEndpoint(rawEndpoint string, secure bool) error {
	if strings.ContainsAny(rawEndpoint, "?#") {
		return errors.New("HTTP endpoint must not include a query string or fragment")
	}
	if strings.Contains(rawEndpoint, "://") {
		endpoint, err := url.Parse(rawEndpoint)
		if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.User != nil {
			return errors.New("HTTP endpoint must be a valid URL without credentials")
		}
		switch endpoint.Scheme {
		case "https":
			if !secure {
				return fmt.Errorf("%s=true is required for HTTPS endpoints", envSecure)
			}
		case "http":
			if secure {
				return fmt.Errorf("%s=false is required for HTTP endpoints", envSecure)
			}
		default:
			return errors.New("HTTP endpoint must use http or https")
		}
		return nil
	}

	endpoint, err := url.Parse("//" + rawEndpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Path != "" && endpoint.Path != "/") {
		return errors.New("HTTP endpoint must be a host:port or a valid URL without credentials")
	}
	return nil
}

func isHostedHTTPEndpoint(rawEndpoint string) bool {
	endpoint := rawEndpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "//" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.stacktrail.com")
}

func isDefaultHostedHTTPEndpoint(rawEndpoint string) bool {
	endpoint := rawEndpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "api.stacktrail.com") {
		return false
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return false
	}
	switch parsed.Path {
	case "", "/", "/v1/traces":
		return true
	default:
		return false
	}
}

func installDefaultClient(client *client) error {
	defaultClient.Lock()
	defer defaultClient.Unlock()
	if defaultClient.client != nil {
		return errors.New("stacktrail is already initialized")
	}
	defaultClient.client = client
	return nil
}

func configuredClient() *client {
	defaultClient.RLock()
	defer defaultClient.RUnlock()
	return defaultClient.client
}

func newClient(ctx context.Context, config sdkConfig) (*client, error) {
	if config.apiKey == "" {
		return nil, errors.New("API key is required")
	}
	switch config.environment {
	case "development", "staging", "production":
	default:
		return nil, fmt.Errorf("%s must be development, staging, or production", envEnvironment)
	}

	if config.serviceName == "" {
		config.serviceName = defaultServiceName()
	}

	var (
		exporter *otlptrace.Exporter
		err      error
	)
	if config.useHTTP {
		exporter, err = newHTTPExporter(ctx, config)
	} else {
		exporter, err = newGRPCExporter(ctx, config)
	}
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(config.serviceName),
			attribute.String("deployment.environment", config.environment),
		),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("create telemetry resource: %w", err)
	}

	spanLimits := sdktrace.NewSpanLimits()
	spanLimits.AttributeValueLengthLimit = maxTelemetryValueRunes
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithRawSpanLimits(spanLimits),
	)

	var beacons *beaconDispatcher
	if endpoint := beaconEndpoint(config); endpoint != "" {
		beacons = newBeaconDispatcher(endpoint, config.apiKey)
	}

	return &client{
		tracer:         provider.Tracer("stacktrail-sdk"),
		tracerProvider: provider,
		environment:    config.environment,
		beacons:        beacons,
	}, nil
}

func defaultServiceName() string {
	executable, err := os.Executable()
	if err != nil {
		return "unknown_service"
	}
	name := filepath.Base(executable)
	if name == "" || name == "." {
		return "unknown_service"
	}
	return name
}

func newHTTPExporter(ctx context.Context, config sdkConfig) (*otlptrace.Exporter, error) {
	httpOpts := []otlptracehttp.Option{
		otlptracehttp.WithHeaders(map[string]string{"X-API-Key": config.apiKey}),
		otlptracehttp.WithHTTPClient(newNoRedirectHTTPClient(otlpRequestTimeout)),
	}

	if strings.Contains(config.collectorEndpoint, "://") {
		endpoint, err := url.Parse(config.collectorEndpoint)
		if err != nil {
			return nil, errors.New("HTTP endpoint must be a valid URL")
		}
		if endpoint.Path == "" || endpoint.Path == "/" {
			endpoint.Path = "/v1/traces"
		}
		httpOpts = append(httpOpts, otlptracehttp.WithEndpointURL(endpoint.String()))
	} else {
		httpOpts = append(httpOpts,
			otlptracehttp.WithEndpoint(config.collectorEndpoint),
			otlptracehttp.WithURLPath("/v1/traces"),
		)
		if config.allowInsecureTransport {
			httpOpts = append(httpOpts, otlptracehttp.WithInsecure())
		}
	}
	return otlptracehttp.New(ctx, httpOpts...)
}

func newGRPCExporter(ctx context.Context, config sdkConfig) (*otlptrace.Exporter, error) {
	grpcOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(config.collectorEndpoint),
		otlptracegrpc.WithHeaders(map[string]string{"X-API-Key": config.apiKey}),
	}
	if config.allowInsecureTransport {
		grpcOpts = append(grpcOpts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.New(ctx, grpcOpts...)
}

// Job represents a background job execution.
type Job struct {
	ctx       context.Context
	span      trace.Span
	startTime time.Time
	endOnce   sync.Once
	client    *client
}

// StartJob starts a job through Stacktrail's package-level client. Call Init
// successfully before using this function.
func StartJob(ctx context.Context, jobName string) *Job {
	client := configuredClient()
	if client == nil {
		panic("stacktrail is not initialized; call stacktrail.Init before StartJob")
	}
	return client.startJob(ctx, jobName)
}

func (c *client) startJob(ctx context.Context, jobName string) *Job {
	jobName = normalizeJobName(jobName)
	ctx, span := c.tracer.Start(ctx, jobName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("job.name", jobName),
			attribute.String("job.type", "background"),
		),
	)
	startedAt := time.Now().UTC()

	if c.beacons != nil {
		spanContext := span.SpanContext()
		if spanContext.IsValid() {
			c.beacons.enqueue(beaconPayload{
				TraceID:     spanContext.TraceID().String(),
				SpanID:      spanContext.SpanID().String(),
				JobName:     jobName,
				StartedAt:   startedAt.Format(time.RFC3339Nano),
				Environment: c.environment,
			})
		}
	}

	return &Job{
		ctx:       ctx,
		span:      span,
		startTime: startedAt,
		client:    c,
	}
}

// AddMetadata adds metadata to the job. Blank keys are ignored.
func (j *Job) AddMetadata(key string, value interface{}) {
	key, ok := normalizeMetadataKey(key)
	if !ok {
		return
	}
	attributeKey := "metadata." + key

	switch v := value.(type) {
	case string:
		j.span.SetAttributes(attribute.String(attributeKey, truncateRunes(v, maxTelemetryValueRunes)))
	case int:
		j.span.SetAttributes(attribute.Int(attributeKey, v))
	case int64:
		j.span.SetAttributes(attribute.Int64(attributeKey, v))
	case float64:
		j.span.SetAttributes(attribute.Float64(attributeKey, v))
	case bool:
		j.span.SetAttributes(attribute.Bool(attributeKey, v))
	default:
		j.span.SetAttributes(attribute.String(attributeKey, truncateRunes(fmt.Sprintf("%v", v), maxTelemetryValueRunes)))
	}
}

// End completes the job. A nil error marks it successful; a non-nil error marks
// it failed. End is safe to call more than once and from concurrent goroutines.
func (j *Job) End(err error) {
	j.endOnce.Do(func() {
		duration := attribute.Int64("job.duration_ms", time.Since(j.startTime).Milliseconds())
		if err == nil {
			j.span.SetStatus(codes.Ok, "Job completed successfully")
			j.span.SetAttributes(attribute.String("job.status", "success"), duration)
		} else {
			details := inspectError(err)
			message := details.Message
			// Record only the redacted message. Passing the original error to
			// RecordError would duplicate potentially sensitive text in the
			// OpenTelemetry exception event.
			j.span.RecordError(errors.New(message))
			j.span.SetStatus(codes.Error, message)
			attributes := []attribute.KeyValue{
				attribute.String("job.status", "failed"),
				duration,
			}
			attributes = append(attributes, details.attributes()...)
			j.span.SetAttributes(attributes...)
		}
		j.span.End()
	})
}

// Success marks the job as successfully completed.
func (j *Job) Success() {
	j.End(nil)
}

// Fail marks the job as failed with an error. A nil error is recorded as an
// unspecified failure instead of panicking.
func (j *Job) Fail(err error) {
	if err == nil {
		err = errors.New("job failed without an error")
	}
	j.End(err)
}

// Context returns the job's context for propagation.
func (j *Job) Context() context.Context {
	return j.ctx
}

// StartChildJob starts a child job for nested job execution.
func (j *Job) StartChildJob(jobName string) *Job {
	return j.client.startJob(j.ctx, jobName)
}

// AddEvent adds an event to the job timeline. Blank names are ignored.
func (j *Job) AddEvent(name string, attributes ...attribute.KeyValue) {
	name, ok := normalizeNonemptyName(name)
	if !ok {
		return
	}
	j.span.AddEvent(name, trace.WithAttributes(attributes...))
}

// ForceFlush exports all spans buffered by the package-level client before ctx
// is cancelled. Call Init first.
func ForceFlush(ctx context.Context) error {
	client := configuredClient()
	if client == nil {
		return errors.New("stacktrail is not initialized")
	}
	return client.ForceFlush(ctx)
}

func (c *client) ForceFlush(ctx context.Context) error {
	return c.tracerProvider.ForceFlush(ctx)
}

// Shutdown flushes and shuts down Stacktrail's package-level client.
func Shutdown(ctx context.Context) error {
	defaultClient.Lock()
	client := defaultClient.client
	defaultClient.client = nil
	defaultClient.Unlock()
	if client == nil {
		return nil
	}
	return client.Shutdown(ctx)
}

func (c *client) Shutdown(ctx context.Context) error {
	if c.beacons == nil {
		return c.tracerProvider.Shutdown(ctx)
	}

	beaconDone := make(chan error, 1)
	go func() {
		beaconDone <- c.beacons.shutdown(ctx)
	}()
	providerErr := c.tracerProvider.Shutdown(ctx)
	return errors.Join(<-beaconDone, providerErr)
}
