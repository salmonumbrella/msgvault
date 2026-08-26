package peoplesweep

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

	got, err := client.Fetch(t.Context())
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

	got, err := client.Fetch(t.Context())
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

	got, err := client.Fetch(ctx)
	close(release)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestNewModelsDevClientDoesNotInheritCallerHTTPConfiguration(t *testing.T) {
	called := &atomic.Bool{}
	callerTransport := &http.Transport{
		Proxy:           func(*http.Request) (*url.URL, error) { return url.Parse("https://user:secret@proxy.invalid") },
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{[]byte(modelsDevRequestCanary)}}}},
	}
	caller := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called.Store(true)
			return nil, errors.New(modelsDevBodyCanary)
		}),
		Jar:           cookieJarWithCanary(t),
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
	}
	client := NewModelsDevClient(caller)
	transport, ok := client.client.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Nil(t, transport.Proxy)
	assert.Empty(t, transport.TLSClientConfig.Certificates)
	assert.Nil(t, client.client.Jar)
	assert.Equal(t, modelsDevTotalTimeout, client.client.Timeout)

	caller.Transport = callerTransport
	caller.Jar = cookieJarWithCanary(t)
	callerTransport.TLSClientConfig.Certificates = append(callerTransport.TLSClientConfig.Certificates, tls.Certificate{})
	assert.NotEqual(t, callerTransport, transport)
	assert.Empty(t, transport.TLSClientConfig.Certificates)
	assert.False(t, called.Load())
}

func TestModelsDevFetchRejectsSizeOverflowByOneAndClosesBody(t *testing.T) {
	closed := &atomic.Bool{}
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(w, zeroReader{}, modelsDevMaxBodyBytes+1)
	}))
	serverClientTransport := client.client.Transport
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := serverClientTransport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body = &closeTrackingBody{ReadCloser: response.Body, closed: closed}
		return response, nil
	})
	t.Cleanup(server.Close)

	got, err := client.Fetch(t.Context())
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
	base := client.client.Transport
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body = &closeTrackingBody{ReadCloser: response.Body, closed: closed}
		return response, nil
	})
	t.Cleanup(server.Close)

	got, err := client.Fetch(t.Context())
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

			got, err := client.Fetch(t.Context())
			require.Error(t, err)
			assert.Nil(t, got)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestModelsDevFetchReturnsStableErrorWithoutPartialSuggestions(t *testing.T) {
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, modelsDevBodyCanary)
	}))
	t.Cleanup(server.Close)
	suggestions, err := client.Fetch(t.Context())
	require.ErrorIs(t, err, ErrModelsDevUnavailable)
	assert.Nil(t, suggestions)
	assert.NotContains(t, err.Error(), modelsDevBodyCanary)
}

func TestModelsDevFetchCancelsDuringTransformationWithoutPartialSuggestions(t *testing.T) {
	body := `{"a":{"id":"a","name":"A","models":{}},"b":{"id":"b","name":"B","models":{}}}`
	client, server := modelsDevTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	client.hooks = &modelsDevHooks{afterProvider: cancel}

	suggestions, err := client.Fetch(ctx)
	require.ErrorIs(t, err, ErrModelsDevTimeout)
	assert.Nil(t, suggestions)
}

func modelsDevTLSFixture(t *testing.T, handler http.Handler) (*ModelsDevClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	certificate := server.Certificate()
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	target := server.Listener.Addr().String()
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	return newModelsDevClientForTest(dial, pool, certificate.DNSNames[0]), server
}

func cookieJarWithCanary(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	catalogURL, err := url.Parse(modelsDevURL)
	require.NoError(t, err)
	jar.SetCookies(catalogURL, []*http.Cookie{{Name: "session", Value: modelsDevRequestCanary}})
	return jar
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
