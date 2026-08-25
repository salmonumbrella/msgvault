package peoplesweep

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxProviderResponseBytes = 1 << 20

type httpDriver struct {
	client *http.Client
}

type httpDriverResponse struct {
	body      []byte
	requestID string
}

type safeTransportError struct {
	operation string
	cause     error
}

func (e *safeTransportError) Error() string { return e.operation }
func (e *safeTransportError) Unwrap() error { return e.cause }

func newHTTPDriver(client *http.Client) *httpDriver {
	if client == nil {
		client = http.DefaultClient
	}
	isolated := *client
	isolated.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &httpDriver{client: &isolated}
}

func (d *httpDriver) post(
	ctx context.Context,
	target string,
	profile ProviderProfile,
	credential Credential,
	body []byte,
) (httpDriverResponse, error) {
	request, err := http.NewRequestWithContext( // #nosec G704 -- the exact operator-configured endpoint is validated by ProviderProfile.
		ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return httpDriverResponse{}, errors.New("create inference provider request")
	}
	request.Header.Set("Content-Type", "application/json")
	if err := applyHTTPCredential(request, profile.Auth, credential); err != nil {
		return httpDriverResponse{}, err
	}

	response, err := d.client.Do(request)
	if err != nil {
		return httpDriverResponse{}, &safeTransportError{
			operation: "call inference provider", cause: err,
		}
	}
	defer disposeHTTPResponse(response.Body)
	requestID := safeRequestID(response.Header)
	if response.StatusCode != http.StatusOK {
		retryAfter := time.Duration(0)
		if retryableProviderStatus(response.StatusCode) {
			retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		}
		return httpDriverResponse{}, &ProviderError{
			StatusCode: response.StatusCode, RequestID: requestID, RetryAfter: retryAfter,
		}
	}

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		if requestErr := request.Context().Err(); requestErr != nil {
			return httpDriverResponse{}, &safeTransportError{
				operation: "read inference provider response", cause: requestErr,
			}
		}
		return httpDriverResponse{}, &safeTransportError{
			operation: "read inference provider response", cause: err,
		}
	}
	if requestErr := request.Context().Err(); requestErr != nil {
		return httpDriverResponse{}, &safeTransportError{
			operation: "read inference provider response", cause: requestErr,
		}
	}
	if len(responseBody) > maxProviderResponseBytes {
		return httpDriverResponse{}, errors.Join(
			ErrInvalidStructuredOutput, errors.New("provider response is too large"))
	}
	return httpDriverResponse{body: responseBody, requestID: requestID}, nil
}

func applyHTTPCredential(request *http.Request, scheme AuthScheme, credential Credential) error {
	if scheme == AuthNone {
		if credential.hasValue() {
			return errors.New("unauthenticated inference provider does not accept a credential")
		}
		return nil
	}
	if credential.Scheme != scheme {
		return errors.New("people provider credential authentication scheme does not match profile")
	}
	value := credential.Value()
	if value == "" {
		return errors.New("inference provider credential is empty")
	}
	if !safeHTTPHeaderValue(value) {
		return errors.New("inference provider credential is not a valid HTTP header value")
	}
	switch scheme {
	case AuthBearer:
		request.Header.Set("Authorization", "Bearer "+value)
	case AuthXAPIKey:
		request.Header.Set("x-api-key", value)
	case AuthGoogleAPIKey:
		request.Header.Set("x-goog-api-key", value)
	default:
		return errors.New("unsupported people provider HTTP authentication scheme")
	}
	return nil
}

func safeHTTPHeaderValue(value string) bool {
	for _, char := range []byte(value) {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func safeRequestID(header http.Header) string {
	for _, name := range []string{"x-request-id", "request-id", "x-amzn-requestid"} {
		if value := header.Get(name); safeProviderMetadata(value) {
			return value
		}
	}
	return ""
}

func disposeHTTPResponse(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 32<<10))
	_ = body.Close()
}

func retryableProviderStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		(status >= http.StatusInternalServerError && status <= 599)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseUint(value, 10, 64)
	if err == nil {
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := when.Sub(now)
	if delay <= 0 {
		return 0
	}
	return delay
}
