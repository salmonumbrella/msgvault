package api

import (
	"context"
	"net/http"
	"net/url"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/store"
)

// IntroductionOperations provides the shared introduction read projection.
type IntroductionOperations interface {
	ListIntroductionsContext(ctx context.Context, grant *agentgrant.Grant, query store.PersonIntroductionQuery) (introops.Page, error)
	GetIntroductionContext(ctx context.Context, grant *agentgrant.Grant, id string) (introops.Record, error)
}

// IntroductionMutations is an optional owner-only capability.
type IntroductionMutations interface {
	PrepareIntroductionContext(ctx context.Context, grant *agentgrant.Grant, prepare store.PersonIntroductionPrepare) (introops.Record, error)
	EditIntroductionContext(ctx context.Context, grant *agentgrant.Grant, id string, edit store.PersonIntroductionEdit) (introops.Record, error)
	CancelIntroductionContext(ctx context.Context, grant *agentgrant.Grant, id string, revision int64) (introops.Record, error)
}

type IntroductionEditRequest struct {
	Revision int64   `json:"revision"`
	Title    *string `json:"title,omitzero"`
	Body     *string `json:"body,omitzero"`
}

type IntroductionRevisionRequest struct {
	Revision int64 `json:"revision"`
}

func (s *Server) registerIntroductionRoutes(api huma.API) {
	list := rawAPIV1Operation("listIntroductions", http.MethodGet, "/intros", "List local introductions")
	list.Parameters = []*huma.Param{contactIntegerParam("source_id", 1, 0), contactIntegerParam("limit", 1, 100), queryStringParam("after_id", "Opaque continuation cursor", false)}
	list.Responses = jsonResponsesFor[introops.Page](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListIntroductions)

	get := rawAPIV1Operation("getIntroduction", http.MethodGet, "/intros/{id}", "Get a local introduction")
	get.Parameters = []*huma.Param{{Name: "id", In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
	get.Responses = jsonResponsesFor[introops.Record](api)
	addErrorResponses(api, get.Responses, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetIntroduction)

	prepare := rawAPIV1Operation("prepareIntroduction", http.MethodPost, "/intros", "Prepare a local introduction")
	prepare.RequestBody = jsonRequestBodyFor[store.PersonIntroductionPrepare](api)
	prepare.Responses = jsonResponsesFor[introops.Record](api, http.StatusCreated)
	addErrorResponses(api, prepare.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, prepare, s.handlePrepareIntroduction)

	edit := rawAPIV1Operation("editIntroduction", http.MethodPatch, "/intros/{id}", "Edit a local introduction")
	edit.Parameters = get.Parameters
	edit.RequestBody = jsonRequestBodyFor[IntroductionEditRequest](api)
	edit.Responses = jsonResponsesFor[introops.Record](api)
	addErrorResponses(api, edit.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, edit, s.handleEditIntroduction)

	cancel := rawAPIV1Operation("cancelIntroduction", http.MethodPost, "/intros/{id}/cancel", "Cancel a local introduction")
	cancel.Parameters = get.Parameters
	cancel.RequestBody = jsonRequestBodyFor[IntroductionRevisionRequest](api)
	cancel.Responses = jsonResponsesFor[introops.Record](api)
	addErrorResponses(api, cancel.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, cancel, s.handleCancelIntroduction)
}

func (s *Server) handleListIntroductions(w http.ResponseWriter, r *http.Request) {
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	source, err := contactQueryInteger(r, "source_id", 1, 0)
	if err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	limit, err := contactQueryInteger(r, "limit", 1, 100)
	if err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	after, err := contactQueryString(r, "after_id")
	if err != nil {
		s.writeIntroductionError(w, store.ErrIntroductionInvalid)
		return
	}
	ops, ok := s.store.(IntroductionOperations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Introduction reads are unavailable")
		return
	}
	grant, release, ok := s.admitIntroductionRead(w, r)
	if !ok {
		return
	}
	defer release()
	page, err := ops.ListIntroductionsContext(r.Context(), grant, store.PersonIntroductionQuery{SourceID: source, Limit: int(limit), AfterID: after})
	if err != nil {
		s.writeIntroductionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetIntroduction(w http.ResponseWriter, r *http.Request) {
	ops, ok := s.store.(IntroductionOperations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Introduction reads are unavailable")
		return
	}
	grant, release, ok := s.admitIntroductionRead(w, r)
	if !ok {
		return
	}
	defer release()
	row, err := ops.GetIntroductionContext(r.Context(), grant, r.PathValue("id"))
	if err != nil {
		s.writeIntroductionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) introductionMutations(w http.ResponseWriter, r *http.Request) (IntroductionMutations, bool) {
	if s.requestAuthentication(r).Grant != nil {
		s.writeIntroductionError(w, introops.ErrForbidden)
		return nil, false
	}
	ops, ok := s.store.(IntroductionMutations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Introduction mutations are unavailable")
	}
	return ops, ok
}

func (s *Server) handlePrepareIntroduction(w http.ResponseWriter, r *http.Request) {
	ops, ok := s.introductionMutations(w, r)
	if !ok {
		return
	}
	var in store.PersonIntroductionPrepare
	if _, ok := decodeEntityRequestFields(w, r, &in, "introduction"); !ok {
		return
	}
	row, err := ops.PrepareIntroductionContext(r.Context(), nil, in)
	if err != nil {
		s.writeIntroductionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) handleEditIntroduction(w http.ResponseWriter, r *http.Request) {
	ops, ok := s.introductionMutations(w, r)
	if !ok {
		return
	}
	var in IntroductionEditRequest
	if _, ok := decodeEntityRequestFields(w, r, &in, "introduction"); !ok {
		return
	}
	row, err := ops.EditIntroductionContext(r.Context(), nil, r.PathValue("id"), store.PersonIntroductionEdit{Revision: in.Revision, Title: in.Title, Body: in.Body})
	if err != nil {
		s.writeIntroductionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleCancelIntroduction(w http.ResponseWriter, r *http.Request) {
	ops, ok := s.introductionMutations(w, r)
	if !ok {
		return
	}
	var in IntroductionRevisionRequest
	if _, ok := decodeEntityRequestFields(w, r, &in, "introduction"); !ok {
		return
	}
	row, err := ops.CancelIntroductionContext(r.Context(), nil, r.PathValue("id"), in.Revision)
	if err != nil {
		s.writeIntroductionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// admitIntroductionRead reserves the shared snapshot budget only for authorized agents.
func (s *Server) admitIntroductionRead(w http.ResponseWriter, r *http.Request) (*agentgrant.Grant, func(), bool) {
	grant := s.requestAuthentication(r).Grant
	if grant == nil {
		return nil, func() {}, true
	}
	if !grant.HasPermission(agentgrant.PermissionDraftCreate) {
		s.writeIntroductionError(w, introops.ErrForbidden)
		return nil, nil, false
	}
	select {
	case s.readSnapshotSlots <- struct{}{}:
	case <-r.Context().Done():
		writeError(w, http.StatusServiceUnavailable, "agent_read_busy", "Agent reads are at capacity; retry later")
		return nil, nil, false
	}
	release := sync.OnceFunc(func() { <-s.readSnapshotSlots })
	if boundary, ok := r.Context().Value(readResponseKey{}).(*readResponseWriter); ok {
		boundary.release = release
	}
	return grant, release, true
}

func (s *Server) writeIntroductionError(w http.ResponseWriter, err error) {
	code := introops.ErrorCode(err)
	status := http.StatusServiceUnavailable
	switch code {
	case "invalid_args", "same_person", "unsupported_source":
		status = http.StatusBadRequest
	case "not_permitted":
		status = http.StatusForbidden
	case "intro_not_found":
		status = http.StatusNotFound
	case "revision_mismatch", "state_conflict", "source_retired":
		status = http.StatusConflict
	}
	writeError(w, status, code, "Introduction request failed")
}
