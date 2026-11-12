package federation

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/netguard"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// ParseHubURL accepts only origins, requiring HTTPS except on loopback.
func ParseHubURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return "", errors.New("hub URL must be an origin")
	}
	origin, err := providercredentials.EndpointOrigin(raw)
	if err != nil {
		return "", err
	}
	if _, err := netguard.TargetPort(u); err != nil {
		return "", err
	}
	if strings.ToLower(u.Scheme) != "https" && !netguard.IsLoopbackHost(u.Hostname()) {
		return "", errors.New("hub URL requires HTTPS outside loopback")
	}
	return origin, nil
}

var errHubRejected = errors.New("hub rejected enrollment")

type httpDoer struct{ client *http.Client }

func (d httpDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return d.client.Do(req.WithContext(ctx))
}

type HubClient struct {
	client          *client.Client
	token, spokeUID string
}

func NewHubClient(origin, token, spokeUID string) (*HubClient, error) {
	httpClient := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c, err := client.New(origin, runtime.WithHTTPClient(httpDoer{client: httpClient}))
	if err != nil {
		return nil, errors.New("cannot create hub client")
	}
	return &HubClient{client: c, token: token, spokeUID: spokeUID}, nil
}

func (h *HubClient) credentials(_ context.Context, r *http.Request) error {
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set(apiprotocol.SpokeInstanceHeader, h.spokeUID)
	return nil
}

func (h *HubClient) handshake(ctx context.Context) (*generated.FederationHandshakeResponse, error) {
	resp, err := h.client.GetFederationHandshakeWithResponse(ctx, h.credentials)
	if err == nil && resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		return nil, errHubRejected
	}
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || resp.JSON200 == nil {
		return nil, errors.New("hub handshake failed")
	}
	return resp.JSON200, nil
}

func (h *HubClient) revoke(ctx context.Context, uid string) error {
	resp, err := h.client.RevokeFederationEnrollmentWithResponse(ctx, &generated.RevokeFederationEnrollmentRequestOptions{PathParams: &generated.RevokeFederationEnrollmentPath{UID: uid}}, h.credentials)
	if err != nil || resp == nil || resp.StatusCode != http.StatusNoContent {
		return errors.New("hub revocation failed")
	}
	return nil
}
