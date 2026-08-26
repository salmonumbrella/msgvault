package peoplesweep

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modelsDevUserAgent     = "OpenAI File Downloader, XaiImageApiFetch/1.0"
	modelsDevRequestCanary = "catalog-request-canary-never-send"
	modelsDevBodyCanary    = "catalog-body-canary-never-report"
)

func TestModelsDevFetchParsesCurrentFixtureDeterministicallyByAPIShape(t *testing.T) {
	fixture, err := os.ReadFile("testdata/modelsdev/catalog.json")
	require.NoError(t, err)
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api.json", r.URL.Path)
		assert.Equal(t, "models.dev", r.Host)
		assert.Equal(t, modelsDevUserAgent, r.UserAgent())
		assert.Empty(t, r.URL.RawQuery)
		assert.Equal(t, int64(0), r.ContentLength)
		for _, name := range []string{"Authorization", "Cookie", "X-API-Key", "X-Goog-API-Key", "X-Config", "X-Host-ID", "X-Archive"} {
			assert.Empty(t, r.Header.Get(name))
		}
		for name, values := range r.Header {
			for _, value := range values {
				assert.NotContains(t, name, modelsDevRequestCanary)
				assert.NotContains(t, value, modelsDevRequestCanary)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	catalogURL, err := url.Parse(modelsDevURL)
	require.NoError(t, err)
	jar.SetCookies(catalogURL, []*http.Cookie{{Name: "session", Value: modelsDevRequestCanary}})
	client.Jar = jar

	got, err := NewModelsDevClient(client).Fetch(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, []string{"alpha", "anthropic-label", "openai-label", "template"},
		[]string{got[0].ID, got[1].ID, got[2].ID, got[3].ID})
	assert.Equal(t, []string{"ALPHA_API_KEY", "ALPHA_TOKEN"}, got[0].EnvironmentNames)
	assert.Equal(t, []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses}, got[0].ProtocolCandidates)
	assert.Equal(t, []Protocol{ProtocolOpenAIChat}, got[1].ProtocolCandidates)
	assert.Equal(t, []string{"302AI_API_KEY", "SHAPE_API_KEY"}, got[1].EnvironmentNames)
	assert.Empty(t, got[2].ProtocolCandidates)
	assert.Equal(t, []Protocol{ProtocolGoogleGenerateContent}, got[3].ProtocolCandidates)
	assert.Equal(t, "${CATALOG_BASE_URL}/v1", got[3].Endpoint)

	require.Len(t, got[0].Models, 1)
	assert.Equal(t, ModelSuggestion{
		ID: "alpha-basic", Name: "Alpha Basic", Reasoning: false, StructuredOutput: true,
		InputCostMicroUSDPerMillionTokens:  int64Pointer(1),
		OutputCostMicroUSDPerMillionTokens: int64Pointer(2_500_000),
	}, got[0].Models[0])
	require.Len(t, got[1].Models, 2)
	assert.Equal(t, "@cf/shape-reasoner", got[1].Models[0].ID)
	assert.Equal(t, int64(132_001), *got[1].Models[0].InputCostMicroUSDPerMillionTokens)
	assert.Equal(t, int64(1_254_001), *got[1].Models[0].OutputCostMicroUSDPerMillionTokens)
	assert.True(t, got[1].Models[0].Reasoning)
	assert.False(t, got[1].Models[0].StructuredOutput)
	assert.Equal(t, "~shape/latest", got[1].Models[1].ID)
	assert.Equal(t, "Shape Latest Alias", got[1].Models[1].Name)
}

func TestModelsDevFetchRejectsRedirectWithoutFollowing(t *testing.T) {
	var calls atomic.Int32
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "https://models.dev/redirected")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	got, err := NewModelsDevClient(client).Fetch(t.Context())
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Equal(t, int32(1), calls.Load())
	assert.NotContains(t, err.Error(), "/redirected")
}

func TestModelsDevFetchHonorsCallerTimeoutWithSafeError(t *testing.T) {
	release := make(chan struct{})
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	got, err := NewModelsDevClient(client).Fetch(ctx)
	close(release)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchAppliesFixedTotalTimeout(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		remaining := time.Until(deadline)
		assert.Greater(t, remaining, 14*time.Second)
		assert.LessOrEqual(t, remaining, 15*time.Second)
		return nil, errors.New(modelsDevBodyCanary)
	})}

	_, err := NewModelsDevClient(client).Fetch(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchRejectsSizeOverflowByOneAndClosesBody(t *testing.T) {
	closed := &atomic.Bool{}
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(w, zeroReader{}, modelsDevMaxBodyBytes+1)
	}))
	serverClientTransport := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := serverClientTransport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body = &closeTrackingBody{ReadCloser: response.Body, closed: closed}
		return response, nil
	})
	t.Cleanup(server.Close)

	got, err := NewModelsDevClient(client).Fetch(t.Context())
	require.Error(t, err)
	assert.Nil(t, got)
	assert.True(t, closed.Load())
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchDrainsAndClosesStatusErrorWithoutBodyDisclosure(t *testing.T) {
	closed := &atomic.Bool{}
	body := strings.Repeat(modelsDevBodyCanary, 1024)
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, body)
	}))
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body = &closeTrackingBody{ReadCloser: response.Body, closed: closed}
		return response, nil
	})
	t.Cleanup(server.Close)

	got, err := NewModelsDevClient(client).Fetch(t.Context())
	require.Error(t, err)
	assert.Nil(t, got)
	assert.True(t, closed.Load())
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchRejectsDuplicateAndUnsafeCatalogData(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "duplicate provider key", body: `{"same":{"id":"same","name":"One","models":{}},"same":{"id":"same","name":"Two","models":{}}}`},
		{name: "duplicate provider id", body: `{"one":{"id":"same","name":"One","models":{}},"two":{"id":"same","name":"Two","models":{}}}`},
		{name: "duplicate model key", body: `{"one":{"id":"one","name":"One","models":{"same":{"id":"same","name":"One"},"same":{"id":"same","name":"Two"}}}}`},
		{name: "duplicate model id", body: `{"one":{"id":"one","name":"One","models":{"first":{"id":"same","name":"One"},"second":{"id":"same","name":"Two"}}}}`},
		{name: "unsafe provider id", body: `{"bad id":{"id":"bad id","name":"Bad","models":{}}}`},
		{name: "unsafe model id", body: `{"one":{"id":"one","name":"One","models":{"bad id":{"id":"bad id","name":"Bad"}}}}`},
		{name: "oversized name", body: `{"one":{"id":"one","name":"` + strings.Repeat("x", 513) + `","models":{}}}`},
		{name: "credentialed URL", body: `{"one":{"id":"one","name":"One","api":"https://user:secret@example.test/v1","models":{}}}`},
		{name: "queried URL", body: `{"one":{"id":"one","name":"One","api":"https://example.test/v1?host=secret","models":{}}}`},
		{name: "remote plaintext URL", body: `{"one":{"id":"one","name":"One","api":"http://example.test/v1","models":{}}}`},
		{name: "unsafe base template suffix", body: `{"one":{"id":"one","name":"One","api":"${BASE_URL}suffix/v1","models":{}}}`},
		{name: "invalid environment", body: `{"one":{"id":"one","name":"One","env":["BAD ENV"],"models":{}}}`},
		{name: "negative price", body: oneModelCatalog(`{"input":-0.1}`)},
		{name: "overflowing price", body: oneModelCatalog(`{"input":9223372036855}`)},
		{name: "invalid price type", body: oneModelCatalog(`{"input":"secret-price"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)

			got, err := NewModelsDevClient(client).Fetch(t.Context())
			require.Error(t, err)
			assert.Nil(t, got)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestModelsDevFetchFailureLeavesCustomSetupRecoverable(t *testing.T) {
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, modelsDevBodyCanary)
	}))
	t.Cleanup(server.Close)
	candidate := capabilityTestCandidate(ProtocolOpenAIChat, "https://custom.example.test/v1")
	wantCandidate := candidate

	suggestions, err := NewModelsDevClient(client).Fetch(t.Context())
	require.Error(t, err)
	assert.Nil(t, suggestions)
	assert.Equal(t, wantCandidate, candidate)
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func modelsDevTLSFixture(t *testing.T, handler http.Handler) (*http.Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	baseTransport, ok := server.Client().Transport.(*http.Transport)
	require.True(t, ok)
	transport := baseTransport.Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // local httptest TLS certificate
	target := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	return &http.Client{Transport: transport}, server
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type closeTrackingBody struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func oneModelCatalog(cost string) string {
	var buffer bytes.Buffer
	_ = json.NewEncoder(&buffer).Encode(map[string]any{
		"one": map[string]any{
			"id": "one", "name": "One", "models": map[string]any{
				"model": json.RawMessage(`{"id":"model","name":"Model","cost":` + cost + `}`),
			},
		},
	})
	return buffer.String()
}

func int64Pointer(value int64) *int64 { return &value }
