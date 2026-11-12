package federation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var errIdentityChanged = errors.New("hub identity changed")
var errHubUnavailable = errors.New("hub unavailable")
var errTokenMissing = errors.New("cannot read enrollment token")

var uidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func tokenPath(dir, uid string) string { return filepath.Join(dir, "federation-"+uid+".token") }
func policyFromResponse(p generated.FederationHubPolicy) config.FederationHubPolicy {
	return config.FederationHubPolicy{Retention: p.Retention, AllowedLevels: p.AllowedLevels, AllowAttachments: p.AllowAttachments, MinGroupSize: int(p.MinGroupSize), Processing: p.Processing}
}

func Join(ctx context.Context, st *store.Store, tokensDir, rawURL, expectedUID, token string) error {
	if !uidPattern.MatchString(expectedUID) {
		return errors.New("hub instance must be 64 lowercase hexadecimal digits")
	}
	if token == "" {
		return errors.New("enrollment token is empty")
	}
	origin, err := ParseHubURL(rawURL)
	if err != nil {
		return err
	}
	links, err := st.ListFederationLinksContext(ctx)
	if err != nil {
		return err
	}
	for _, l := range links {
		if l.HubOrigin == origin || l.HubArchiveUID == expectedUID {
			return errors.New("hub is already linked")
		}
	}
	spokeUID, err := st.ArchiveUIDContext(ctx)
	if err != nil {
		return err
	}
	h, err := NewHubClient(origin, token, spokeUID)
	if err != nil {
		return err
	}
	response, err := h.handshake(ctx)
	if err != nil {
		return err
	}
	if response.HubInstance != expectedUID {
		return errors.New("hub instance does not match")
	}
	supported := false
	for _, version := range response.ProtocolVersions {
		if version == 1 {
			supported = true
		}
	}
	if !supported || response.EnrollmentState != "active" {
		return errors.New("hub enrollment or protocol is unsupported")
	}
	policy := policyFromResponse(response.Policy)
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("invalid hub policy: %w", err)
	}
	digest, err := policy.PolicyDigest()
	if err != nil {
		return errors.New("cannot digest hub policy")
	}
	if digest != response.PolicyDigest {
		return errors.New("hub policy digest does not match")
	}
	if response.EnrollmentUID == "" || response.Member == "" {
		return errors.New("incomplete hub enrollment")
	}
	data, err := json.Marshal(policy, json.Deterministic(true))
	if err != nil {
		return errors.New("cannot encode hub policy")
	}
	if strings.Contains(response.Member, token) || strings.Contains(response.EnrollmentUID, token) || strings.Contains(string(data), token) {
		return errors.New("hub response contains the enrollment token")
	}
	link := store.FederationLink{HubArchiveUID: expectedUID, HubOrigin: origin, EnrollmentUID: response.EnrollmentUID, Member: response.Member, PolicyJSON: string(data), PolicyDigest: digest}
	if err := st.CreateFederationLinkContext(ctx, link); err != nil {
		return fmt.Errorf("save federation link: %w", err)
	}
	if err = fileutil.SecureMkdirAll(tokensDir, 0700); err == nil {
		err = fileutil.SecureReplaceFile(tokenPath(tokensDir, expectedUID), []byte(token), 0600)
	} else {
		err = fmt.Errorf("create token directory: %w", err)
	}
	if err != nil {
		// Cleanup must finish even if the join was canceled.
		if deleteErr := st.DeleteFederationLinkContext(context.WithoutCancel(ctx), expectedUID); deleteErr != nil {
			return fmt.Errorf("save enrollment token: %w; remove link: %w", err, deleteErr)
		}
		return fmt.Errorf("save enrollment token: %w", err)
	}
	return nil
}

type LinkStatus struct {
	HubOrigin string                     `json:"hub_origin"`
	Member    string                     `json:"member"`
	State     string                     `json:"state"`
	Shares    int                        `json:"shares"`
	Policy    config.FederationHubPolicy `json:"policy"`
}

func hubForLink(ctx context.Context, st *store.Store, tokensDir string, l store.FederationLink) (*HubClient, error) {
	token, err := os.ReadFile(tokenPath(tokensDir, l.HubArchiveUID))
	if err != nil {
		return nil, errTokenMissing
	}
	spokeUID, err := st.ArchiveUIDContext(ctx)
	if err != nil {
		return nil, err
	}
	return NewHubClient(l.HubOrigin, string(token), spokeUID)
}

func checkIdentity(ctx context.Context, st *store.Store, h *HubClient, l store.FederationLink) error {
	response, err := h.handshake(ctx)
	if errors.Is(err, errHubRejected) {
		return errHubRejected
	}
	if err != nil {
		return errHubUnavailable
	}
	if response.HubInstance != l.HubArchiveUID {
		if err := st.SetFederationLinkStateContext(ctx, l.HubArchiveUID, "identity_changed"); err != nil {
			return err
		}
		return errIdentityChanged
	}
	return nil
}

func Status(ctx context.Context, st *store.Store, tokensDir string) ([]LinkStatus, error) {
	links, err := st.ListFederationLinksContext(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]LinkStatus, 0, len(links))
	for _, l := range links {
		status := LinkStatus{HubOrigin: l.HubOrigin, Member: l.Member, State: l.State}
		if err := json.Unmarshal([]byte(l.PolicyJSON), &status.Policy); err != nil {
			return nil, errors.New("invalid stored hub policy")
		}
		if l.State == "active" {
			h, err := hubForLink(ctx, st, tokensDir, l)
			if err == nil {
				err = checkIdentity(ctx, st, h, l)
			}
			if err != nil {
				if errors.Is(err, errIdentityChanged) {
					status.State = "identity_changed"
				} else if errors.Is(err, errTokenMissing) {
					status.State = "token_missing"
				} else if errors.Is(err, errHubRejected) {
					status.State = "rejected"
				} else if errors.Is(err, errHubUnavailable) {
					status.State = "unreachable"
				} else {
					return nil, fmt.Errorf("record hub identity: %w", err)
				}
			} else {
				status.State = "enrolled"
			}
		}
		result = append(result, status)
	}
	return result, nil
}

func Leave(ctx context.Context, st *store.Store, tokensDir, rawURL string, localOnly bool) (string, error) {
	origin, err := ParseHubURL(rawURL)
	if err != nil {
		return "", err
	}
	links, err := st.ListFederationLinksContext(ctx)
	if err != nil {
		return "", err
	}
	for _, l := range links {
		if l.HubOrigin != origin {
			continue
		}
		if !localOnly {
			if l.State != "active" {
				return "", errors.New("hub identity changed; use --local-only")
			}
			h, err := hubForLink(ctx, st, tokensDir, l)
			if err == nil {
				err = checkIdentity(ctx, st, h, l)
			}
			if err == nil {
				err = h.revoke(ctx, l.EnrollmentUID)
			}
			if err != nil {
				return "", fmt.Errorf("leave not confirmed; use --local-only: %w", err)
			}
		}
		path := tokenPath(tokensDir, l.HubArchiveUID)
		token, readErr := os.ReadFile(path)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return "", errors.New("cannot read token; use --local-only")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove token; use --local-only: %w", err)
		}
		if err := st.DeleteFederationLinkContext(ctx, l.HubArchiveUID); err != nil {
			if readErr == nil {
				if restoreErr := fileutil.SecureReplaceFile(path, token, 0600); restoreErr != nil {
					return "", errors.New("remove link failed and token could not be restored; use --local-only")
				}
			}
			return "", fmt.Errorf("remove link; use --local-only: %w", err)
		}
		return l.EnrollmentUID, nil
	}
	return "", errors.New("hub is not linked")
}
