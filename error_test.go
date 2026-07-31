package stacktrail

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type structuredTestError struct {
	status    int
	code      string
	retryable bool
}

func (e structuredTestError) Error() string {
	return "request for person@example.com failed with token=secret-canary"
}
func (e structuredTestError) HTTPStatusCode() int { return e.status }
func (e structuredTestError) ErrorCode() string   { return e.code }
func (e structuredTestError) Retryable() bool     { return e.retryable }

func TestInspectErrorExtractsCanonicalFacts(t *testing.T) {
	err := WithErrorDetails(structuredTestError{
		status:    422,
		code:      "validation_error",
		retryable: false,
	}, ErrorDetails{Service: "resend"})

	details := inspectError(err)
	if details.Service != "resend" || details.Code != "validation_error" {
		t.Fatalf("unexpected identity facts: %#v", details)
	}
	if details.HTTPStatus != 422 || details.Category != ErrorCategoryValidation {
		t.Fatalf("unexpected classification: %#v", details)
	}
	if details.Retryable == nil || *details.Retryable {
		t.Fatalf("unexpected retryability: %#v", details.Retryable)
	}
	for _, sensitive := range []string{"person@example.com", "secret-canary"} {
		if strings.Contains(details.Message, sensitive) {
			t.Fatalf("message exposed %q: %q", sensitive, details.Message)
		}
	}
}

func TestInspectErrorRecognizesContextFailures(t *testing.T) {
	timeout := inspectError(context.DeadlineExceeded)
	if timeout.Category != ErrorCategoryTimeout || timeout.Retryable == nil || !*timeout.Retryable {
		t.Fatalf("deadline classification: %#v", timeout)
	}
	cancelled := inspectError(context.Canceled)
	if cancelled.Category != ErrorCategoryCancelled || cancelled.Retryable == nil || *cancelled.Retryable {
		t.Fatalf("cancelled classification: %#v", cancelled)
	}
}

func TestRedactErrorMessageRemovesCommonSecretsAndPII(t *testing.T) {
	message := redactErrorMessage(`POST https://user:pass@example.test/send?api_key=query-secret Authorization: Bearer bearer-secret email=person@example.com password=hunter2 token=token-secret eyJabcdefgh.abcdefgh.abcdefgh`)
	for _, sensitive := range []string{
		"user:pass", "query-secret", "bearer-secret", "person@example.com",
		"hunter2", "token-secret", "eyJabcdefgh.abcdefgh.abcdefgh",
	} {
		if strings.Contains(message, sensitive) {
			t.Fatalf("redaction exposed %q in %q", sensitive, message)
		}
	}
	for _, marker := range []string{"[REDACTED]", "[REDACTED_EMAIL]", "[REDACTED_TOKEN]"} {
		if !strings.Contains(message, marker) {
			t.Fatalf("redaction marker %q missing from %q", marker, message)
		}
	}
}

func TestJobExportsOnlyRedactedStructuredError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	client := &client{tracer: provider.Tracer("test"), tracerProvider: provider}

	job := client.startJob(context.Background(), "structured-failure")
	job.Fail(WithErrorDetails(structuredTestError{
		status: 503,
		code:   "provider_unavailable",
	}, ErrorDetails{Service: "mail-provider"}))

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans", len(spans))
	}
	span := spans[0]
	attributes := make(map[string]any)
	for _, item := range span.Attributes {
		attributes[string(item.Key)] = item.Value.AsInterface()
	}
	for key, want := range map[string]any{
		"job.error.category":    "unavailable",
		"job.error.code":        "provider_unavailable",
		"job.error.http_status": int64(503),
		"job.error.retryable":   false,
		"job.error.service":     "mail-provider",
	} {
		if got := attributes[key]; got != want {
			t.Errorf("%s=%#v, want %#v", key, got, want)
		}
	}
	serialized := span.Status.Description
	for _, event := range span.Events {
		serialized += event.Name
		for _, item := range event.Attributes {
			serialized += item.Value.String()
		}
	}
	for _, sensitive := range []string{"person@example.com", "secret-canary"} {
		if strings.Contains(serialized, sensitive) {
			t.Fatalf("span exposed %q", sensitive)
		}
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown provider: %v", err)
	}
}

func TestWithErrorDetailsPreservesErrorChain(t *testing.T) {
	original := errors.New("original")
	if !errors.Is(WithErrorDetails(original, ErrorDetails{}), original) {
		t.Fatal("structured wrapper did not preserve errors.Is")
	}
}

func TestInspectErrorRecognizesStructuredJSONResponse(t *testing.T) {
	err := WithErrorDetails(
		errors.New(`{"name":"validation_error","message":"Invalid recipient person@example.com","statusCode":422}`),
		ErrorDetails{Service: "resend"},
	)
	details := inspectError(err)
	if details.Service != "resend" || details.Code != "validation_error" || details.HTTPStatus != 422 {
		t.Fatalf("structured JSON facts: %#v", details)
	}
	if details.Category != ErrorCategoryValidation {
		t.Fatalf("structured JSON category: %q", details.Category)
	}
	if strings.Contains(details.Message, "person@example.com") {
		t.Fatalf("structured JSON message exposed PII: %q", details.Message)
	}
}

func TestInspectPlainErrorDoesNotInventOptionalFacts(t *testing.T) {
	details := inspectError(errors.New("plain operational failure"))
	if details.Service != "" || details.Code != "" {
		t.Fatalf("plain error invented service=%q code=%q", details.Service, details.Code)
	}
	if details.Category != ErrorCategoryUnknown || details.Retryable != nil {
		t.Fatalf("plain error invented classification: %#v", details)
	}
}
