package requestsign

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type signingTransport struct {
	base   *url.URL
	keyID  string
	secret []byte
	max    int64
	next   http.RoundTripper
}

// NewTransport signs every native transport attempt after request editors run.
// It pins the origin and mount before dispatch, including direct and streaming requests.
func NewTransport(base, keyID string, secret []byte, maxBytes int64, next http.RoundTripper) (http.RoundTripper, error) {
	u, err := ValidateBase(base)
	if err != nil {
		return nil, err
	}
	if !ValidID(keyID) || len(secret) != 64 {
		return nil, ErrInvalidSignature
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxRequestBytes
	}
	if maxBytes < 1 || maxBytes > MaxRequestBytes {
		return nil, ErrBodyTooLarge
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &signingTransport{base: u, keyID: keyID, secret: append([]byte(nil), secret...), max: maxBytes, next: next}, nil
}

func (t *signingTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	u, err := ValidateTarget(original.URL.String())
	if err != nil || u.Scheme != t.base.Scheme || u.Host != t.base.Host || original.Host != "" && original.Host != t.base.Host || t.base.Path != "" && u.Path != t.base.Path && !strings.HasPrefix(u.Path, t.base.Path+"/") {
		if original.Body != nil {
			_ = original.Body.Close()
		}
		return nil, errors.New("signed request is outside configured HTTPS origin or prefix")
	}
	ctx, cancel := context.WithTimeout(original.Context(), 5*time.Minute)
	r := original.Clone(ctx)
	if err := sign(r, t.secret, t.keyID, time.Now, NewNonce(), t.max); err != nil {
		cancel()
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	// A lost response must not let net/http resend an admitted nonce. Keep even
	// empty bodies non-sentinel so HTTP/1 and HTTP/2 cannot rewind this attempt.
	// Explicit native retries return through this transport and receive a new nonce.
	r.GetBody = nil
	if r.Body == nil || r.Body == http.NoBody {
		r.Body = io.NopCloser(bytes.NewReader(nil))
	}
	response, err := t.next.RoundTrip(r)
	if err != nil {
		cancel()
		_ = r.Body.Close()
		return nil, err
	}
	response.Body = &deadlineBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type deadlineBody struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (b *deadlineBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }
