package sync

import (
	"context"
	"errors"
	"net/http"

	imaplib "github.com/emersion/go-imap/v2"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

var errNativeSourcePermissionReduction = errors.New("observe native source permission denial")

type nativeMessageReadPermission interface {
	NativeMessageReadPermission() (read, known bool)
}

// nativeProfileWithPermission pins the account before provider IO, then binds
// the authenticated native identity and current native grant to that lifetime.
// An empty successful sync is not permission evidence. IMAP uses its separate
// authenticated session and mailbox epoch authority.
func (s *Syncer) nativeProfileWithPermission(ctx context.Context, source *store.Source) (*gmail.Profile, error) {
	if source.SourceType == sourceTypeIMAP {
		return s.nativeIMAPProfileWithPermission(ctx, source)
	}
	if source.SourceType != sourceTypeGmail {
		return s.client.GetProfile(ctx)
	}
	resource, err := s.store.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return nil, err
	}
	if resource.SourceType == "" && source.SourceType == sourceTypeGmail && store.EqualIdentifier(resource.ProviderID, source.Identifier) {
		// FullWithFinalizer normalizes legacy empty source types in memory. Keep
		// syncing that selected row; an untyped archive is not native Events evidence.
		return s.client.GetProfile(ctx)
	}
	if resource.SourceType != source.SourceType || !store.EqualIdentifier(resource.ProviderID, source.Identifier) {
		return nil, errors.New("native_authority_unavailable")
	}
	observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceTypeGmail, NativeIdentity: source.Identifier}
	s.nativeGmailSourceID = source.ID
	// Scope membership authority is attached later, after a native label
	// inventory has been admitted. Profile success alone cannot authorize it.
	s.nativeGmailSourceReference = ""
	s.nativeGmailScopePins = nil
	s.nativeGmailPermissionReference = resource.Reference
	s.nativeGmailInitiallyReadGrant = false
	s.nativeGmailPermissionReduced = false
	s.nativeGmailNativeIdentity = source.Identifier
	if _, native := s.client.(nativeMessageReadPermission); !native {
		// Events-only grant checks must preserve ordinary archival semantics.
		// Generic archive adapters cannot establish native permission evidence.
		if err := s.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
			return nil, err
		}
		return s.client.GetProfile(ctx)
	}

	previouslyAllowed, err := s.store.NativeSourceMessagePermission(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if previouslyAllowed {
		observation.Known, observation.MessageRead = true, true
	}
	s.nativeSourcePermission = &observation
	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		return nil, errors.Join(err, s.observeNativeSourceDenial(ctx, err), s.observeNativeSourcePermissionReduction(ctx))
	}
	if profile == nil {
		return nil, errors.Join(errors.New("read authenticated Gmail identity: provider returned no profile"), s.observeNativeSourcePermissionReduction(ctx))
	}
	if _, err := provideridentity.GmailProfileEvidence(source, profile.EmailAddress); err != nil {
		// A foreign authenticated response cannot grant authority to this
		// account or preserve a previous grant. Fail before archive ingestion.
		observation.Known, observation.MessageRead = true, false
		reduction := s.store.ObserveNativeSourcePermission(ctx, observation)
		if !s.store.MCPMessageCaptureEnabled(sourceTypeGmail) {
			// Retain ordinary alias/profile-change archival behavior when this
			// source is unselected, while retiring its mismatched Events grant.
			return profile, reduction
		}
		return nil, errors.Join(errors.New("native_authority_unavailable"), reduction)
	}
	if native, ok := s.client.(nativeMessageReadPermission); ok {
		observation.MessageRead, observation.Known = native.NativeMessageReadPermission()
	}
	if err := s.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
		return nil, err
	}
	s.nativeGmailInitiallyReadGrant = observation.Known && observation.MessageRead
	return profile, nil
}

// Reduce only the source lifetime pinned before provider IO, through this run's
// scoped Store. Only an observation failure is returned; callers retain the
// provider error. Quota and transport failures do not establish revocation.
func (s *Syncer) observeNativeSourceDenial(ctx context.Context, cause error) error {
	if s.nativeSourcePermission == nil || cause == nil {
		return nil
	}
	revoked := false
	if refresh, ok := errors.AsType[*oauth2.RetrieveError](cause); ok {
		revoked = refresh.ErrorCode == "invalid_grant"
	}
	if status, ok := errors.AsType[*gmail.StatusError](cause); ok {
		revoked = revoked || status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden
	}
	if status, ok := errors.AsType[*imaplib.Error](cause); ok {
		revoked = revoked || status.Code == imaplib.ResponseCodeAuthenticationFailed || status.Code == imaplib.ResponseCodeAuthorizationFailed
	}
	if !revoked {
		return nil
	}
	observation := *s.nativeSourcePermission
	observation.Known, observation.MessageRead = true, false
	reduction := s.store.ObserveNativeSourcePermission(ctx, observation)
	if reduction == nil {
		// The original source reference has been retired. Do not reread and adopt
		// its replacement if another item in the same batch reports denial.
		s.nativeSourcePermission = nil
	}
	if reduction != nil {
		return errors.Join(errNativeSourcePermissionReduction, reduction)
	}
	return nil
}

// observeNativeSourcePermissionReduction checks token metadata after a
// provider request. A successful OAuth refresh can narrow Gmail grants even
// when the request itself succeeds, so persist a known read loss against the
// source lifetime pinned before provider IO and stop this run before it can
// publish more work under the old grant.
func (s *Syncer) observeNativeSourcePermissionReduction(ctx context.Context) error {
	pinned := s.nativeSourcePermission
	if pinned == nil || !pinned.Known || !pinned.MessageRead {
		return nil
	}
	native, ok := s.client.(nativeMessageReadPermission)
	if !ok {
		return nil
	}
	messageRead, known := native.NativeMessageReadPermission()
	if known && messageRead {
		return nil
	}

	observation := *pinned
	observation.Known = known
	observation.MessageRead = messageRead
	if err := s.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
		return errors.Join(errNativeSourcePermissionReduction, err)
	}
	// The source lifetime has been retired. Do not reread and adopt its
	// replacement if another item in the same batch observes a denial.
	s.nativeSourcePermission = nil
	return errNativeSourcePermissionReduction
}

// nativeGmailPermissionNarrowedAfterRefresh checks for a known loss after a
// Gmail call that may refresh the native token. The initial positive probe
// determines whether a later negative observation is a reduction.
func (s *Syncer) nativeGmailPermissionNarrowedAfterRefresh() bool {
	if !s.nativeGmailInitiallyReadGrant || s.nativeGmailPermissionReduced {
		return false
	}
	native, ok := s.client.(nativeMessageReadPermission)
	if !ok {
		return false
	}
	messageRead, known := native.NativeMessageReadPermission()
	return known && !messageRead
}

// persistNativeGmailPermissionReduction revokes the run's pinned Events
// lifetime and drops scope pins before subsequent message writes can reuse it.
func (s *Syncer) persistNativeGmailPermissionReduction(ctx context.Context) error {
	if s.nativeGmailPermissionReduced || s.nativeGmailSourceID <= 0 || s.nativeGmailPermissionReference == "" {
		return nil
	}
	if err := s.store.ObserveNativeSourcePermission(ctx, store.NativeSourcePermission{
		SourceID:        s.nativeGmailSourceID,
		SourceReference: s.nativeGmailPermissionReference,
		SourceType:      sourceTypeGmail,
		NativeIdentity:  s.nativeGmailNativeIdentity,
		Known:           true,
	}); err != nil {
		return err
	}
	s.nativeGmailPermissionReduced = true
	s.nativeGmailSourceReference = ""
	s.nativeGmailPermissionReference = ""
	s.nativeGmailScopePins = nil
	s.nativeGmailInitiallyReadGrant = false
	s.nativeSourcePermission = nil
	return nil
}

// observeNativeGmailPermissionReduction records a known loss of message-read
// scope immediately after successful Gmail history requests.
func (s *Syncer) observeNativeGmailPermissionReduction(ctx context.Context) error {
	if !s.nativeGmailPermissionNarrowedAfterRefresh() {
		return nil
	}
	return s.persistNativeGmailPermissionReduction(ctx)
}

// nativeGmailPermissionFailure applies an authorization loss observed after
// the initial profile probe through the source reference pinned for this run.
// The observer never reloads the resource, so a stale request cannot revoke a
// replacement source lifetime.
func (s *Syncer) nativeGmailPermissionFailure(ctx context.Context, cause error) error {
	if cause == nil || s.nativeGmailPermissionReduced || s.nativeGmailSourceID <= 0 || s.nativeGmailPermissionReference == "" {
		return cause
	}
	denied := false
	if refresh, ok := errors.AsType[*oauth2.RetrieveError](cause); ok && refresh.ErrorCode == "invalid_grant" {
		denied = true
	}
	if status, ok := errors.AsType[*gmail.StatusError](cause); ok {
		denied = denied || status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden
	}
	if !denied {
		return cause
	}
	observation := store.NativeSourcePermission{SourceID: s.nativeGmailSourceID, SourceReference: s.nativeGmailPermissionReference, SourceType: sourceTypeGmail, NativeIdentity: s.nativeGmailNativeIdentity, Known: true}
	return errors.Join(cause, s.store.ObserveNativeSourcePermission(ctx, observation))
}

func (s *Syncer) nativeMailPermissionFailure(ctx context.Context, cause error) error {
	if cause == nil {
		return nil
	}
	if s.nativeSourcePermission != nil {
		return errors.Join(cause, s.observeNativeSourceDenial(ctx, cause), s.observeNativeSourcePermissionReduction(ctx))
	}
	return s.nativeGmailPermissionFailure(ctx, cause)
}

// PinNativeGmailPermissionObserver captures the selected Gmail source lifetime
// for manual command setup, which may fail before a Syncer can run its profile
// probe. Returned failures preserve their typed causes for normal error handling.
func PinNativeGmailPermissionObserver(ctx context.Context, st *store.Store, source *store.Source) (func(context.Context, error) error, error) {
	if source == nil || source.SourceType != sourceTypeGmail {
		return nil, errors.New("not a Gmail source")
	}
	resource, err := st.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return nil, err
	}
	if resource.SourceType != sourceTypeGmail || !store.EqualIdentifier(resource.ProviderID, source.Identifier) {
		return nil, errors.New("native_authority_unavailable")
	}
	observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceTypeGmail, NativeIdentity: source.Identifier, Known: true}
	return func(failureCtx context.Context, cause error) error {
		if cause == nil {
			return nil
		}
		denied := false
		if refresh, ok := errors.AsType[*oauth2.RetrieveError](cause); ok {
			denied = refresh.ErrorCode == "invalid_grant"
		}
		if status, ok := errors.AsType[*gmail.StatusError](cause); ok {
			denied = denied || status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden
		}
		if !denied {
			return cause
		}
		return errors.Join(cause, st.ObserveNativeSourcePermission(failureCtx, observation))
	}, nil
}
