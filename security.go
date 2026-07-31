package stacktrail

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxTelemetryNameRunes  = 256
	maxMetadataKeyRunes    = 128
	maxTelemetryValueRunes = 4096
	otlpRequestTimeout     = 10 * time.Second
)

var errInvalidGRPCEndpoint = errors.New("gRPC endpoint must be a valid host:port")

func newNoRedirectHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func validateGRPCEndpoint(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return errInvalidGRPCEndpoint
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errInvalidGRPCEndpoint
	}
	return nil
}

func normalizeJobName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "unnamed-job"
	}
	return truncateRunes(name, maxTelemetryNameRunes)
}
func normalizeNonemptyName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	return truncateRunes(name, maxTelemetryNameRunes), true
}

func normalizeMetadataKey(key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	return truncateRunes(key, maxMetadataKeyRunes), true
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}
