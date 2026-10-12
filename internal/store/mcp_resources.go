package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"
)

// MCPResource exposes native identity and presentation without configuration or
// private lifetime keys. Wire adapters render its integer IDs as decimal strings.
type MCPResource struct {
	Kind                                           string
	ID, SourceID                                   int64
	SourceType, ProviderID, DisplayName, Reference string
	ScopeKind, ParentProviderID, Epoch             string
}

// CanonicalMCPResourceReference reports whether a reference has its exact wire
// encoding. It does not authenticate the reference or authorize a resource.
func CanonicalMCPResourceReference(reference string) bool {
	if len(reference) != 48 || !strings.HasPrefix(reference, "mcp1.") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(reference[5:])
	return err == nil && len(raw) == sha256.Size && base64.RawURLEncoding.EncodeToString(raw) == reference[5:]
}

func mcpResourceMAC(generation string, fields []string) (string, error) {
	key, err := hex.DecodeString(generation)
	if err != nil || len(key) != 32 {
		return "", mcpStoreError("resource_unavailable")
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func mcpResourceReference(resource MCPResource, generation, parent string) (string, error) {
	mac, err := mcpResourceMAC(generation, []string{"msgvault/resource/v1", resource.Kind, strconv.FormatInt(resource.ID, 10), strconv.FormatInt(resource.SourceID, 10), resource.SourceType, resource.ProviderID, parent})
	if err != nil {
		return "", err
	}
	return "mcp1." + mac, nil
}

func (s *Store) mcpResource(ctx context.Context, tx *sql.Tx, kind string, id int64, create bool) (MCPResource, error) {
	resource := MCPResource{Kind: kind, ID: id}
	var lifetime mcpScopeLifetime
	if id <= 0 {
		return resource, mcpStoreError("invalid_resource_reference")
	}
	var identifier string
	var err error
	switch kind {
	case mcpScopeSource:
		resource.SourceID = id
		// #nosec G701 -- Fixed SQL; the exact archive source identity is bound.
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT source_type,identifier,COALESCE(display_name,'') FROM sources WHERE id=?`), id).Scan(&resource.SourceType, &resource.ProviderID, &resource.DisplayName)
	case mcpScopeConversation:
		// #nosec G701 -- Fixed SQL; the exact archive resource identity is bound.
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT c.source_id,s.source_type,COALESCE(c.source_conversation_id,''),COALESCE(c.title,''),s.identifier FROM conversations c JOIN sources s ON s.id=c.source_id WHERE c.id=?`), id).Scan(&resource.SourceID, &resource.SourceType, &resource.ProviderID, &resource.DisplayName, &identifier)
	default:
		return resource, mcpStoreError("invalid_resource_reference")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return resource, mcpStoreError("resource_unavailable")
	}
	if err != nil {
		return resource, err
	}
	if create {
		lifetime, err = s.ensureMCPScopeLifetime(ctx, tx, kind, id, resource.SourceID)
	} else {
		lifetime, err = s.mcpScopeLifetime(ctx, tx, kind, id, resource.SourceID)
	}
	if err != nil {
		return resource, err
	}
	var parent string
	if kind == mcpScopeConversation {
		source := MCPResource{Kind: mcpScopeSource, ID: resource.SourceID, SourceID: resource.SourceID, SourceType: resource.SourceType, ProviderID: identifier}
		parent, err = mcpResourceReference(source, lifetime.source, "")
		if err != nil {
			return resource, err
		}
	}
	resource.Reference, err = mcpResourceReference(resource, lifetime.scope, parent)
	return resource, err
}

// GetMCPResource resolves one native resource under the identity/source/Events
// fences, creating its private lifetime only when missing. It does no provider IO.
func (s *Store) GetMCPResource(ctx context.Context, kind string, id int64) (MCPResource, error) {
	var resource MCPResource
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		var err error
		resource, err = s.mcpResource(ctx, tx, kind, id, true)
		return err
	})
	return resource, err
}
