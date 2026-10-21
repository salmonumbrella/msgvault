package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
)

const MCPDiscoveryMinAPISchemaVersion = InboxMinAPISchemaVersion

var ErrMCPCapabilitiesUnavailable = errors.New("MCP operation discovery unavailable")

// MCPCapabilities reads current caller admission without following redirects or
// observing a provider. Version eligibility alone never admits a control call.
func (c *Client) MCPCapabilities(ctx context.Context) (*apiprotocol.MCPCapabilities, error) {
	version, err := c.mcpSchemaVersionForAdmission(ctx)
	if err != nil {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	return c.MCPCapabilitiesForSchemaVersion(ctx, version)
}

// MCPCapabilitiesForSchemaVersion reads the caller's MCP capabilities using a
// schema version already obtained from authenticated health. The version is
// stable for this client and seeds later admission checks, while each call
// still reads the live caller capability descriptor.
func (c *Client) MCPCapabilitiesForSchemaVersion(ctx context.Context, version string) (*apiprotocol.MCPCapabilities, error) {
	version = c.rememberMCPSchemaVersion(version)
	if !strings.HasPrefix(version, "3.") || !APISchemaVersionAtLeast(version, MCPDiscoveryMinAPISchemaVersion) {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodGet, "/api/v1/mcp/capabilities", nil, &transport, 1<<20)
	if err != nil {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	const wireLimit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, wireLimit+1))
	if err != nil || len(body) > wireLimit {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	var descriptor apiprotocol.MCPCapabilities
	if json.Unmarshal(body, &descriptor) != nil || descriptor.Version != 1 || descriptor.Routes == nil ||
		len(descriptor.Routes) > 256 || len(descriptor.InboxOperations) > 64 {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	if !validMCPCapabilities(&descriptor) {
		return nil, ErrMCPCapabilitiesUnavailable
	}
	return &descriptor, nil
}

// mcpSchemaVersionForAdmission serializes the initial health probe so parallel
// control requests share one stable schema version for the lifetime of the
// client. Failed probes are retried by the next caller.
func (c *Client) mcpSchemaVersionForAdmission(ctx context.Context) (string, error) {
	c.mcpSchemaMu.Lock()
	defer c.mcpSchemaMu.Unlock()
	if c.mcpSchemaKnown {
		return c.mcpSchemaVersion, nil
	}
	version, err := c.APISchemaVersion(ctx)
	if err != nil {
		return "", err
	}
	c.mcpSchemaVersion = version
	c.mcpSchemaKnown = true
	return version, nil
}

func (c *Client) rememberMCPSchemaVersion(version string) string {
	c.mcpSchemaMu.Lock()
	defer c.mcpSchemaMu.Unlock()
	if c.mcpSchemaKnown {
		return c.mcpSchemaVersion
	}
	c.mcpSchemaVersion = version
	c.mcpSchemaKnown = true
	return version
}

var mcpDescriptorIdentity = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func validMCPCapabilities(capabilities *apiprotocol.MCPCapabilities) bool {
	seen := make(map[string]bool, len(capabilities.Routes))
	for _, route := range capabilities.Routes {
		if !mcpDescriptorIdentity.MatchString(route.OperationID) || len(route.OperationID) > 256 ||
			seen[route.OperationID] || len(route.Path) > 2048 ||
			!strings.HasPrefix(route.Path, "/api/v1/") ||
			strings.ContainsAny(route.Path, "?#\\ ") || strings.Contains(route.Path, "/../") ||
			len(route.QueryParameters)+len(route.RequestProperties) > 256 {
			return false
		}
		switch route.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return false
		}
		seen[route.OperationID] = true
		if !validMCPDescriptorNames(route.QueryParameters) || !validMCPDescriptorNames(route.RequestProperties) {
			return false
		}
	}
	seen = make(map[string]bool, len(capabilities.InboxOperations))
	for _, operation := range capabilities.InboxOperations {
		if !mcpDescriptorIdentity.MatchString(operation) || len(operation) > 256 || seen[operation] {
			return false
		}
		seen[operation] = true
	}
	return true
}

func validMCPDescriptorNames(names []string) bool {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !mcpDescriptorIdentity.MatchString(name) || len(name) > 256 || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
