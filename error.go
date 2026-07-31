package stacktrail

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
)

// ErrorCategory is a provider-neutral failure classification.
type ErrorCategory string

const (
	ErrorCategoryUnknown        ErrorCategory = "unknown"
	ErrorCategoryValidation     ErrorCategory = "validation"
	ErrorCategoryAuthentication ErrorCategory = "authentication"
	ErrorCategoryAuthorization  ErrorCategory = "authorization"
	ErrorCategoryNotFound       ErrorCategory = "not_found"
	ErrorCategoryConflict       ErrorCategory = "conflict"
	ErrorCategoryRateLimit      ErrorCategory = "rate_limit"
	ErrorCategoryTimeout        ErrorCategory = "timeout"
	ErrorCategoryUnavailable    ErrorCategory = "unavailable"
	ErrorCategoryCancelled      ErrorCategory = "cancelled"
	ErrorCategoryInternal       ErrorCategory = "internal"
)

// ErrorDetails contains safe, provider-neutral facts about a failure.
// Message is always redacted before export. Retryable is a pointer so false
// can be distinguished from an unknown value.
type ErrorDetails struct {
	Message    string
	Type       string
	Service    string
	Code       string
	Category   ErrorCategory
	HTTPStatus int
	Retryable  *bool
}

// ErrorDetailsProvider can be implemented by application or provider adapters.
type ErrorDetailsProvider interface {
	StacktrailErrorDetails() ErrorDetails
}

type detailedError struct {
	err     error
	details ErrorDetails
}

func (e *detailedError) Error() string { return e.err.Error() }
func (e *detailedError) Unwrap() error { return e.err }
func (e *detailedError) StacktrailErrorDetails() ErrorDetails {
	return e.details
}

// WithErrorDetails attaches provider-neutral facts while preserving the
// original error for errors.Is/errors.As. It is useful for provider adapters
// whose error type does not expose standard status/code methods.
func WithErrorDetails(err error, details ErrorDetails) error {
	if err == nil {
		err = errors.New("job failed without an error")
	}
	if details.Type == "" {
		details.Type = errorTypeName(err)
	}
	return &detailedError{err: err, details: details}
}

type httpStatusCodeProvider interface{ HTTPStatusCode() int }
type statusCodeProvider interface{ StatusCode() int }
type errorCodeProvider interface{ ErrorCode() string }
type codeProvider interface{ Code() string }
type retryableProvider interface{ Retryable() bool }
type temporaryProvider interface{ Temporary() bool }
type categoryProvider interface{ ErrorCategory() ErrorCategory }
type serviceProvider interface{ ErrorService() string }

var (
	emailPattern            = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	jwtPattern              = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	bearerPattern           = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`)
	basicPattern            = regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9+/=]+`)
	urlCredentialPattern    = regexp.MustCompile(`(?i)(https?://)[^/@\s:]+:[^/@\s]+@`)
	secretAssignmentPattern = regexp.MustCompile(`(?i)(["']?(?:api[_-]?key|authorization|access[_-]?token|refresh[_-]?token|token|secret|password|passwd)["']?\s*[:=]\s*["']?)[^\s,"';&]+`)
	sensitiveQueryPattern   = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password)\s*=)[^&\s]+`)
)

func redactErrorMessage(message string) string {
	message = urlCredentialPattern.ReplaceAllString(message, `${1}[REDACTED]@`)
	message = bearerPattern.ReplaceAllString(message, `${1}[REDACTED]`)
	message = basicPattern.ReplaceAllString(message, `${1}[REDACTED]`)
	message = jwtPattern.ReplaceAllString(message, `[REDACTED_TOKEN]`)
	message = sensitiveQueryPattern.ReplaceAllString(message, `${1}[REDACTED]`)
	message = secretAssignmentPattern.ReplaceAllString(message, `${1}[REDACTED]`)
	message = emailPattern.ReplaceAllString(message, `[REDACTED_EMAIL]`)
	message = strings.TrimSpace(message)
	if message == "" {
		message = "job failed without an error message"
	}
	return truncateRunes(message, maxTelemetryValueRunes)
}

func inspectError(err error) ErrorDetails {
	if err == nil {
		err = errors.New("job failed without an error")
	}
	details := ErrorDetails{
		Message:  redactErrorMessage(err.Error()),
		Type:     errorTypeName(err),
		Category: ErrorCategoryUnknown,
	}

	for current := err; current != nil; current = errors.Unwrap(current) {
		mergeProvidedDetails(&details, current)
		mergeStandardDetails(&details, current)
		mergeReflectedDetails(&details, current)
		mergeJSONErrorDetails(&details, current.Error())
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		details.Category = ErrorCategoryTimeout
	case errors.Is(err, context.Canceled):
		details.Category = ErrorCategoryCancelled
	default:
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			details.Category = ErrorCategoryTimeout
		}
	}
	if details.Category == ErrorCategoryUnknown {
		details.Category = categoryFromHTTPStatus(details.HTTPStatus)
	}
	if details.Category == ErrorCategoryUnknown {
		details.Category = categoryFromCode(details.Code)
	}
	if details.Retryable == nil {
		if retryable, known := defaultRetryability(details.Category, details.HTTPStatus); known {
			details.Retryable = boolPointer(retryable)
		}
	}

	details.Message = redactErrorMessage(details.Message)
	details.Type = normalizeErrorToken(details.Type)
	details.Service = normalizeErrorToken(details.Service)
	details.Code = normalizeErrorToken(details.Code)
	return details
}

func mergeProvidedDetails(details *ErrorDetails, err error) {
	provider, ok := err.(ErrorDetailsProvider)
	if !ok {
		return
	}
	provided := provider.StacktrailErrorDetails()
	if provided.Message != "" {
		details.Message = provided.Message
	}
	if provided.Type != "" {
		details.Type = provided.Type
	}
	if provided.Service != "" {
		details.Service = provided.Service
	}
	if provided.Code != "" {
		details.Code = provided.Code
	}
	if isValidErrorCategory(provided.Category) {
		details.Category = provided.Category
	}
	if provided.HTTPStatus >= 100 && provided.HTTPStatus <= 599 {
		details.HTTPStatus = provided.HTTPStatus
	}
	if provided.Retryable != nil {
		value := *provided.Retryable
		details.Retryable = &value
	}
}

func mergeStandardDetails(details *ErrorDetails, err error) {
	if provider, ok := err.(httpStatusCodeProvider); ok {
		details.HTTPStatus = validHTTPStatus(provider.HTTPStatusCode())
	} else if provider, ok := err.(statusCodeProvider); ok {
		details.HTTPStatus = validHTTPStatus(provider.StatusCode())
	}
	if provider, ok := err.(errorCodeProvider); ok {
		details.Code = provider.ErrorCode()
	} else if provider, ok := err.(codeProvider); ok {
		details.Code = provider.Code()
	}
	if provider, ok := err.(retryableProvider); ok {
		details.Retryable = boolPointer(provider.Retryable())
	} else if provider, ok := err.(temporaryProvider); ok {
		details.Retryable = boolPointer(provider.Temporary())
	}
	if provider, ok := err.(categoryProvider); ok && isValidErrorCategory(provider.ErrorCategory()) {
		details.Category = provider.ErrorCategory()
	}
	if provider, ok := err.(serviceProvider); ok {
		details.Service = provider.ErrorService()
	}
}

func mergeReflectedDetails(details *ErrorDetails, err error) {
	value := reflect.ValueOf(err)
	typeOfError := reflect.TypeOf(err)
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return
	}
	if details.Service == "" {
		details.Service = externalServiceName(typeOfError)
	}
	if details.HTTPStatus == 0 {
		for _, name := range []string{"HTTPStatusCode", "StatusCode"} {
			if field := value.FieldByName(name); field.IsValid() && field.CanInt() {
				details.HTTPStatus = validHTTPStatus(int(field.Int()))
				if details.HTTPStatus != 0 {
					break
				}
			}
		}
	}
	if details.Code == "" {
		for _, name := range []string{"ErrorCode", "Code", "Name", "Type"} {
			if field := value.FieldByName(name); field.IsValid() && field.Kind() == reflect.String {
				details.Code = field.String()
				if strings.TrimSpace(details.Code) != "" {
					break
				}
			}
		}
	}
	if details.Retryable == nil {
		if field := value.FieldByName("Retryable"); field.IsValid() && field.Kind() == reflect.Bool {
			details.Retryable = boolPointer(field.Bool())
		}
	}
}

func externalServiceName(errorType reflect.Type) string {
	for errorType != nil && errorType.Kind() == reflect.Pointer {
		errorType = errorType.Elem()
	}
	if errorType == nil {
		return ""
	}
	path := errorType.PkgPath()
	if !strings.Contains(path, ".") {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return ""
	}
	repository := parts[2]
	repository = strings.TrimSuffix(repository, "-go")
	repository = strings.TrimSuffix(repository, "-go-v2")
	return repository
}

func errorTypeName(err error) string {
	typeOfError := reflect.TypeOf(err)
	for typeOfError != nil && typeOfError.Kind() == reflect.Pointer {
		typeOfError = typeOfError.Elem()
	}
	if typeOfError == nil {
		return "error"
	}
	if typeOfError.Name() == "" {
		return "error"
	}
	return typeOfError.Name()
}

func normalizeErrorToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = redactErrorMessage(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._:/-", r) {
			return r
		}
		return '_'
	}, value)
	value = strings.Trim(value, "_")
	return truncateRunes(value, maxMetadataKeyRunes)
}

func validHTTPStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

func categoryFromHTTPStatus(status int) ErrorCategory {
	switch status {
	case 400, 405, 406, 411, 413, 415, 422:
		return ErrorCategoryValidation
	case 401:
		return ErrorCategoryAuthentication
	case 403:
		return ErrorCategoryAuthorization
	case 404, 410:
		return ErrorCategoryNotFound
	case 408, 504:
		return ErrorCategoryTimeout
	case 409:
		return ErrorCategoryConflict
	case 429:
		return ErrorCategoryRateLimit
	}
	if status >= 500 && status <= 599 {
		return ErrorCategoryUnavailable
	}
	return ErrorCategoryUnknown
}

func categoryFromCode(code string) ErrorCategory {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(code))
	switch {
	case strings.Contains(normalized, "rate_limit"), strings.Contains(normalized, "throttl"):
		return ErrorCategoryRateLimit
	case strings.Contains(normalized, "timeout"), strings.Contains(normalized, "deadline"):
		return ErrorCategoryTimeout
	case strings.Contains(normalized, "validation"), strings.Contains(normalized, "invalid"), strings.Contains(normalized, "bad_request"):
		return ErrorCategoryValidation
	case strings.Contains(normalized, "unauthenticated"), strings.Contains(normalized, "authentication"):
		return ErrorCategoryAuthentication
	case strings.Contains(normalized, "unauthorized"), strings.Contains(normalized, "forbidden"), strings.Contains(normalized, "permission"):
		return ErrorCategoryAuthorization
	case strings.Contains(normalized, "not_found"):
		return ErrorCategoryNotFound
	case strings.Contains(normalized, "conflict"), strings.Contains(normalized, "already_exists"):
		return ErrorCategoryConflict
	case strings.Contains(normalized, "cancel"):
		return ErrorCategoryCancelled
	case strings.Contains(normalized, "unavailable"), strings.Contains(normalized, "overload"):
		return ErrorCategoryUnavailable
	case strings.Contains(normalized, "internal"):
		return ErrorCategoryInternal
	default:
		return ErrorCategoryUnknown
	}
}

func defaultRetryability(category ErrorCategory, status int) (bool, bool) {
	if status == 408 || status == 429 || status >= 500 {
		return true, true
	}
	switch category {
	case ErrorCategoryTimeout, ErrorCategoryRateLimit, ErrorCategoryUnavailable:
		return true, true
	case ErrorCategoryValidation, ErrorCategoryAuthentication, ErrorCategoryAuthorization,
		ErrorCategoryNotFound, ErrorCategoryConflict, ErrorCategoryCancelled:
		return false, true
	default:
		return false, false
	}
}

func isValidErrorCategory(category ErrorCategory) bool {
	switch category {
	case ErrorCategoryUnknown, ErrorCategoryValidation, ErrorCategoryAuthentication,
		ErrorCategoryAuthorization, ErrorCategoryNotFound, ErrorCategoryConflict,
		ErrorCategoryRateLimit, ErrorCategoryTimeout, ErrorCategoryUnavailable,
		ErrorCategoryCancelled, ErrorCategoryInternal:
		return true
	default:
		return false
	}
}

func boolPointer(value bool) *bool { return &value }

func (d ErrorDetails) attributes() []attribute.KeyValue {
	attributes := []attribute.KeyValue{
		attribute.Int("job.error.version", 1),
		attribute.String("job.error.message", d.Message),
		attribute.String("job.error.type", d.Type),
		attribute.String("job.error.category", string(d.Category)),
	}
	if d.Service != "" {
		attributes = append(attributes, attribute.String("job.error.service", d.Service))
	}
	if d.Code != "" {
		attributes = append(attributes, attribute.String("job.error.code", d.Code))
	}
	if d.HTTPStatus != 0 {
		attributes = append(attributes, attribute.Int("job.error.http_status", d.HTTPStatus))
	}
	if d.Retryable != nil {
		attributes = append(attributes, attribute.Bool("job.error.retryable", *d.Retryable))
	}
	return attributes
}

func mergeJSONErrorDetails(details *ErrorDetails, message string) {
	message = strings.TrimSpace(message)
	if len(message) == 0 || len(message) > maxTelemetryValueRunes*4 || message[0] != '{' {
		return
	}
	var payload struct {
		Message    string `json:"message"`
		Name       string `json:"name"`
		Code       string `json:"code"`
		Type       string `json:"type"`
		StatusCode int    `json:"statusCode"`
		HTTPStatus int    `json:"http_status"`
		Retryable  *bool  `json:"retryable"`
	}
	if err := json.Unmarshal([]byte(message), &payload); err != nil {
		return
	}
	if payload.Message != "" {
		details.Message = payload.Message
	}
	if details.Code == "" {
		for _, code := range []string{payload.Code, payload.Name, payload.Type} {
			if strings.TrimSpace(code) != "" {
				details.Code = code
				break
			}
		}
	}
	if details.HTTPStatus == 0 {
		details.HTTPStatus = validHTTPStatus(payload.StatusCode)
		if details.HTTPStatus == 0 {
			details.HTTPStatus = validHTTPStatus(payload.HTTPStatus)
		}
	}
	if details.Retryable == nil && payload.Retryable != nil {
		details.Retryable = boolPointer(*payload.Retryable)
	}
}
