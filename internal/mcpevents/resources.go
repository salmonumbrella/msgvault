package mcpevents

import (
	"context"
	"strconv"
)

// ResourceQuery resolves the lifetime of one known import source. Broader
// source and inbox discovery is a separate capability.
type ResourceQuery struct {
	Kind     string `json:"kind" enum:"source"`
	SourceID string `json:"source_id"`
}
type ResourceCapabilities struct {
	Events            []string `json:"events"`
	Read              []string `json:"read"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}
type Resource struct {
	Kind         string               `json:"kind"`
	ID           string               `json:"id"`
	SourceID     string               `json:"source_id"`
	SourceType   string               `json:"source_type"`
	ProviderID   string               `json:"provider_id"`
	DisplayName  string               `json:"display_name"`
	Reference    string               `json:"reference"`
	Capabilities ResourceCapabilities `json:"capabilities"`
}
type ResourcePage struct {
	Resources []Resource `json:"resources"`
}

// DiscoverResources resolves only one known source for exact import admission.
// The owner is authenticated separately; this never contacts a provider or
// lists source, conversation, or inbox membership.
func (s *Service) DiscoverResources(ctx context.Context, principal string, query ResourceQuery) (ResourcePage, error) {
	result := ResourcePage{Resources: make([]Resource, 0)}
	if s.opts.OwnerKey == "" || principal != s.principal {
		return result, &Error{Code: -32012, Reason: "owner_required"}
	}
	sourceID, err := decimal(query.SourceID)
	if query.Kind != "source" || err != nil || sourceID <= 0 {
		return result, invalid("invalid_resource_query")
	}
	err = s.operation(ctx, func() error {
		row, err := s.st.GetMCPResource(ctx, "source", sourceID)
		if err != nil {
			return err
		}
		capabilities := ResourceCapabilities{Events: []string{}, Read: []string{"get_message", "list_thread"}, UnavailableReason: "events_disabled"}
		if s.opts.Enabled {
			capabilities.UnavailableReason = "import_notifications_disabled"
			if s.opts.ImportNotifications {
				if _, _, err := s.st.ValidateMCPEventScope(ctx, importFamily, "source", sourceID, []string{messageKind}); err != nil {
					return err
				}
				capabilities.Events = []string{importFamily}
				capabilities.UnavailableReason = ""
			}
		}
		result.Resources = append(result.Resources, Resource{Kind: "source", ID: strconv.FormatInt(row.ID, 10), SourceID: strconv.FormatInt(row.SourceID, 10), SourceType: row.SourceType, ProviderID: row.ProviderID, DisplayName: row.DisplayName, Reference: row.Reference, Capabilities: capabilities})
		return nil
	})
	if err != nil {
		return ResourcePage{}, safeStoreError(err)
	}
	return result, nil
}
