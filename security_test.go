package stacktrail

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNoRedirectHTTPClientDoesNotForwardCredentials(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	request, err := http.NewRequest(http.MethodPost, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-API-Key", "secret-canary")
	response, err := newNoRedirectHTTPClient(time.Second).Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	_ = response.Body.Close()
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests", got)
	}
}

func TestValidateGRPCEndpoint(t *testing.T) {
	for _, endpoint := range []string{"localhost:4317", "collector.example:443", "[::1]:4317"} {
		if err := validateGRPCEndpoint(endpoint); err != nil {
			t.Errorf("expected %q to be valid: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"localhost", "https://localhost:4317", ":4317", "localhost:0", "localhost:65536"} {
		if err := validateGRPCEndpoint(endpoint); err == nil {
			t.Errorf("expected %q to be invalid", endpoint)
		}
	}
}

func TestEndpointErrorsDoNotEchoSecrets(t *testing.T) {
	for _, endpoint := range []string{
		"https://user:password@collector.example/v1/traces",
		"https://collector.example/v1/traces?token=secret-canary",
	} {
		err := validateHTTPEndpoint(endpoint, true)
		if err == nil {
			t.Fatalf("expected %q to be invalid", endpoint)
		}
		for _, secret := range []string{"password", "secret-canary"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error exposed %q: %v", secret, err)
			}
		}
	}
}

func TestTelemetryNormalizationBoundsValues(t *testing.T) {
	if got := normalizeJobName("   "); got != "unnamed-job" {
		t.Fatalf("blank job normalized to %q", got)
	}
	if got := normalizeJobName(strings.Repeat("界", maxTelemetryNameRunes+1)); len([]rune(got)) != maxTelemetryNameRunes {
		t.Fatalf("job name has %d runes", len([]rune(got)))
	}
	if _, ok := normalizeMetadataKey("   "); ok {
		t.Fatal("blank metadata key was accepted")
	}
}
func TestCanonicalHostedEndpointRecognition(t *testing.T) {
	for _, endpoint := range []string{
		"api.stacktrail.com",
		"api.stacktrail.com:443",
		"https://api.stacktrail.com",
		"https://api.stacktrail.com:443/",
		"https://api.stacktrail.com/v1/traces",
	} {
		if !isDefaultHostedHTTPEndpoint(endpoint) {
			t.Errorf("expected %q to be hosted", endpoint)
		}
	}
	for _, endpoint := range []string{
		"http://api.stacktrail.com",
		"https://api.stacktrail.com:8443",
		"https://api.stacktrail.com/custom",
		"https://collector.example/v1/traces",
	} {
		if isDefaultHostedHTTPEndpoint(endpoint) {
			t.Errorf("expected %q not to be hosted", endpoint)
		}
	}
}
