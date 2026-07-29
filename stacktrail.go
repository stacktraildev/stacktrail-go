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
	envAPIKey            = "STACKTRAIL_API_KEY"
	envServiceName       = "STACKTRAIL_SERVICE_NAME"
	envTransport         = "STACKTRAIL_TRANSPORT"
	envCollectorEndpoint = "STACKTRAIL_COLLECTOR_ENDPOINT"
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
	apiKey         string
}

// Config holds advanced configuration for a private or local OpenTelemetry
// collector. The API key determines the Stacktrail project.
type Config struct {
	APIKey                 string // Stacktrail API key. It is used only for exporter authentication.
	CollectorEndpoint      string // Optional custom OTLP endpoint for advanced or collector-based setups.
	ServiceName            string // Emitting service name; defaults to the executable name.
	UseHTTP                bool   // Use HTTP instead of gRPC (defaults to false).
	AllowInsecureTransport bool   // Allow a non-TLS connection; use only for local development.
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
	return initWithConfig(ctx, config)
}

// InitWithConfig configures Stacktrail's package-level client for advanced
// private or local OpenTelemetry collector setups.
func InitWithConfig(ctx context.Context, config Config) error {
	return initWithConfig(ctx, config)
}

func configFromEnv() (Config, error) {
	config := Config{
		APIKey:            strings.TrimSpace(os.Getenv(envAPIKey)),
		ServiceName:       strings.TrimSpace(os.Getenv(envServiceName)),
		CollectorEndpoint: strings.TrimSpace(os.Getenv(envCollectorEndpoint)),
		UseHTTP:           true,
	}

	if transport, ok := os.LookupEnv(envTransport); ok && strings.TrimSpace(transport) != "" {
		switch strings.ToLower(strings.TrimSpace(transport)) {
		case "http", "https":
			config.UseHTTP = true
		case "grpc":
			config.UseHTTP = false
		default:
			return Config{}, fmt.Errorf("%s must be http or grpc", envTransport)
		}
	}

	secure, err := secureEnv()
	if err != nil {
		return Config{}, err
	}
	if config.UseHTTP && config.CollectorEndpoint != "" {
		return Config{}, fmt.Errorf("%s requires %s=grpc", envCollectorEndpoint, envTransport)
	}
	if config.UseHTTP && !secure {
		return Config{}, fmt.Errorf("%s=false requires %s=grpc", envSecure, envTransport)
	}
	config.AllowInsecureTransport = !secure
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

func initWithConfig(ctx context.Context, config Config) error {
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

func newClient(ctx context.Context, config Config) (*client, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("API key is required")
	}

	if config.CollectorEndpoint == "" {
		if config.UseHTTP {
			config.CollectorEndpoint = defaultHostedEndpoint
		} else {
			config.CollectorEndpoint = "localhost:4317"
		}
	}
	if config.ServiceName == "" {
		config.ServiceName = defaultServiceName()
	}

	var (
		exporter *otlptrace.Exporter
		err      error
	)
	if config.UseHTTP {
		exporter, err = newHTTPExporter(ctx, config)
	} else {
		exporter, err = newGRPCExporter(ctx, config)
	}
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(config.ServiceName)),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("create telemetry resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	return &client{
		tracer:         provider.Tracer("stacktrail-sdk"),
		tracerProvider: provider,
		apiKey:         config.APIKey,
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

func newHTTPExporter(ctx context.Context, config Config) (*otlptrace.Exporter, error) {
	httpOpts := []otlptracehttp.Option{
		otlptracehttp.WithHeaders(map[string]string{"X-API-Key": config.APIKey}),
	}

	if strings.Contains(config.CollectorEndpoint, "://") {
		endpoint, err := url.Parse(config.CollectorEndpoint)
		if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
			return nil, fmt.Errorf("HTTP endpoint must be a valid URL: %q", config.CollectorEndpoint)
		}
		if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
			return nil, fmt.Errorf("HTTP endpoint must use http or https: %q", config.CollectorEndpoint)
		}
		if endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, fmt.Errorf("HTTP endpoint must not include a query string or fragment: %q", config.CollectorEndpoint)
		}
		if endpoint.Scheme == "http" && !config.AllowInsecureTransport {
			return nil, fmt.Errorf("HTTP endpoint uses insecure transport; allow it only for local development")
		}
		if endpoint.Path == "" || endpoint.Path == "/" {
			endpoint.Path = "/v1/traces"
		}
		httpOpts = append(httpOpts, otlptracehttp.WithEndpointURL(endpoint.String()))
	} else {
		httpOpts = append(httpOpts,
			otlptracehttp.WithEndpoint(config.CollectorEndpoint),
			otlptracehttp.WithURLPath("/v1/traces"),
		)
	}

	if config.AllowInsecureTransport {
		httpOpts = append(httpOpts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, httpOpts...)
}

func newGRPCExporter(ctx context.Context, config Config) (*otlptrace.Exporter, error) {
	if strings.Contains(config.CollectorEndpoint, "://") {
		return nil, fmt.Errorf("gRPC endpoint must be host:port, not a URL: %q", config.CollectorEndpoint)
	}

	grpcOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(config.CollectorEndpoint),
		otlptracegrpc.WithHeaders(map[string]string{"X-API-Key": config.APIKey}),
	}
	if config.AllowInsecureTransport {
		grpcOpts = append(grpcOpts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.New(ctx, grpcOpts...)
}

// Job represents a background job execution.
type Job struct {
	ctx        context.Context
	span       trace.Span
	startTime  time.Time
	metadata   map[string]interface{}
	metadataMu sync.Mutex
	endOnce    sync.Once
	client     *client
}

// StartJob starts a job through Stacktrail's package-level client. Call Init
// or InitWithConfig successfully before using this function.
func StartJob(ctx context.Context, jobName string) *Job {
	client := configuredClient()
	if client == nil {
		panic("stacktrail is not initialized; call stacktrail.Init before StartJob")
	}
	return client.startJob(ctx, jobName)
}

func (c *client) startJob(ctx context.Context, jobName string) *Job {
	ctx, span := c.tracer.Start(ctx, jobName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("job.name", jobName),
			attribute.String("job.type", "background"),
		),
	)

	return &Job{
		ctx:       ctx,
		span:      span,
		startTime: time.Now(),
		metadata:  make(map[string]interface{}),
		client:    c,
	}
}

// AddMetadata adds metadata to the job.
func (j *Job) AddMetadata(key string, value interface{}) {
	j.metadataMu.Lock()
	j.metadata[key] = value
	j.metadataMu.Unlock()

	switch v := value.(type) {
	case string:
		j.span.SetAttributes(attribute.String(fmt.Sprintf("metadata.%s", key), v))
	case int:
		j.span.SetAttributes(attribute.Int(fmt.Sprintf("metadata.%s", key), v))
	case int64:
		j.span.SetAttributes(attribute.Int64(fmt.Sprintf("metadata.%s", key), v))
	case float64:
		j.span.SetAttributes(attribute.Float64(fmt.Sprintf("metadata.%s", key), v))
	case bool:
		j.span.SetAttributes(attribute.Bool(fmt.Sprintf("metadata.%s", key), v))
	default:
		j.span.SetAttributes(attribute.String(fmt.Sprintf("metadata.%s", key), fmt.Sprintf("%v", v)))
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
			j.span.RecordError(err)
			j.span.SetStatus(codes.Error, err.Error())
			j.span.SetAttributes(
				attribute.String("job.status", "failed"),
				attribute.String("job.error", err.Error()),
				duration,
			)
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

// AddEvent adds an event to the job timeline.
func (j *Job) AddEvent(name string, attributes ...attribute.KeyValue) {
	j.span.AddEvent(name, trace.WithAttributes(attributes...))
}

// ForceFlush exports all spans buffered by the package-level client before ctx
// is cancelled. Call Init or InitWithConfig first.
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
	return c.tracerProvider.Shutdown(ctx)
}
