package store_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type introFixture struct {
	f                     *storetest.Fixture
	projector             *activity.Projector
	owner, sender, target int64
	person                *store.Person
}

func newIntroFixture(t *testing.T) *introFixture {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	sender := f.EnsureParticipant("sam@example.test", "Sam", "example.test")
	target := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(target)
	require.NoError(t, err)
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	require.NoError(t, err)
	return &introFixture{f, projector, owner, sender, target, person}
}
func (f *introFixture) message(t *testing.T, name string, sender int64, at time.Time, to ...int64) int64 {
	t.Helper()
	msg := f.f.NewMessage().WithSourceMessageID(name).WithSentAt(at).Build()
	msg.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.f.Store.PersistMessage(&store.MessagePersistData{Message: msg})
	require.NoError(t, err)
	require.NoError(t, f.f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
	if to == nil {
		to = []int64{f.owner, f.target}
	}
	require.NoError(t, f.f.Store.ReplaceMessageRecipients(id, "to", to, nil))
	return id
}
func (f *introFixture) run(t *testing.T) {
	t.Helper()
	_, err := f.projector.RunOnce(t.Context())
	require.NoError(t, err)
}
func (f *introFixture) values(t *testing.T, history bool) []store.PersonAttributeValue {
	t.Helper()
	values, err := f.f.Store.ListPersonAttributeValuesContext(t.Context(), f.person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugIntroducedBy, IncludeHistory: history})
	require.NoError(t, err)
	return values
}
func TestIntroducedByReconcilesSavedSenderAndHistory(t *testing.T) {
	f := newIntroFixture(t)
	at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	first := f.message(t, "first", f.sender, at)
	f.run(t)
	assert.Empty(t, f.values(t, false))
	sam, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.run(t)
	values := f.values(t, false)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, sam.ID, *values[0].Value.RecordID)
	assert.Equal(t, store.ProvenanceSystem, values[0].Source)
	require.NotNil(t, values[0].SourceRef)
	assert.Equal(t, fmt.Sprintf("first-contact:message:%d", first), *values[0].SourceRef)
	f.run(t)
	assert.Len(t, f.values(t, true), 1)
	other := f.f.EnsureParticipant("riley@example.test", "Riley", "example.test")
	riley, _, err := f.f.Store.CreatePersonFromParticipant(other)
	require.NoError(t, err)
	older := f.message(t, "older", other, at.Add(-time.Hour))
	f.run(t)
	values = f.values(t, true)
	require.Len(t, values, 2)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, riley.ID, *values[0].Value.RecordID)
	assert.NotNil(t, values[1].SupersededAt)
	_, err = f.f.Store.DB().ExecContext(t.Context(), f.f.Store.Rebind(`DELETE FROM messages WHERE id = ?`), older)
	require.NoError(t, err)
	f.run(t)
	values = f.values(t, false)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, sam.ID, *values[0].Value.RecordID)
}
func TestIntroducedByClearAndUserOverride(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		source                      store.Provenance
		initialJob, clear, override bool
	}{
		{"system clear", store.ProvenanceSystem, true, true, false},
		{"user override", store.ProvenanceUser, true, false, true},
		{"user clear", store.ProvenanceUser, false, true, false},
		{"vcard", store.ProvenanceVCardImport, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntroFixture(t)
			sender, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
			require.NoError(t, err)
			manualID := f.f.EnsureParticipant("manual@example.test", "Manual", "example.test")
			manual, _, err := f.f.Store.CreatePersonFromParticipant(manualID)
			require.NoError(t, err)
			at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
			if tc.initialJob {
				f.message(t, "intro", f.sender, at)
				f.run(t)
			}
			var valueID int64
			if !tc.initialJob || tc.override {
				recordType := "person"
				write, err := f.f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: &recordType, RecordID: &manual.ID}, Source: tc.source})
				require.NoError(t, err)
				valueID = write.Value.ID
			} else {
				values := f.values(t, false)
				require.Len(t, values, 1)
				valueID = values[0].ID
			}
			if !tc.initialJob {
				f.message(t, "intro", f.sender, at)
				f.run(t)
				values := f.values(t, false)
				require.Len(t, values, 1)
				assert.Equal(t, valueID, values[0].ID)
			}
			if tc.clear {
				_, err = f.f.Store.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &valueID})
				require.NoError(t, err)
			}
			f.message(t, "older", f.sender, at.Add(-time.Hour))
			f.run(t)
			values := f.values(t, false)
			if tc.clear {
				assert.Empty(t, values)
			} else {
				require.Len(t, values, 1)
				assert.Equal(t, valueID, values[0].ID)
				require.NotNil(t, values[0].Value.RecordID)
				assert.Equal(t, manual.ID, *values[0].Value.RecordID)
				assert.NotEqual(t, sender.ID, *values[0].Value.RecordID)
			}
		})
	}
}
func TestIntroducedByDisabledBacklog(t *testing.T) {
	f := newIntroFixture(t)
	_, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	def, err := f.f.Store.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, store.AttributeSlugIntroducedBy)
	require.NoError(t, err)
	off := false
	def, err = f.f.Store.UpdateAttributeDefinitionContext(t.Context(), def.ID, def.Revision, store.AttributeDefinitionUpdate{IsActive: &off})
	require.NoError(t, err)
	f.run(t)
	assert.Empty(t, f.values(t, false))
	var dirty sql.NullTime
	err = f.f.Store.DB().QueryRowContext(t.Context(), f.f.Store.Rebind(`SELECT dirty_at FROM person_first_contacts WHERE person_id = ?`), f.person.ID).Scan(&dirty)
	require.NoError(t, err)
	assert.True(t, dirty.Valid)
	on := true
	_, err = f.f.Store.UpdateAttributeDefinitionContext(t.Context(), def.ID, def.Revision, store.AttributeDefinitionUpdate{IsActive: &on})
	require.NoError(t, err)
	f.run(t)
	assert.Len(t, f.values(t, false), 1)
}

func TestIntroducedByOwnerOutreachAndRetirement(t *testing.T) {
	f := newIntroFixture(t)
	owner, _, err := f.f.Store.CreatePersonFromParticipant(f.owner)
	require.NoError(t, err)
	f.message(t, "outreach", f.owner, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), f.target)
	f.run(t)
	values := f.values(t, false)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, owner.ID, *values[0].Value.RecordID)
	second := f.f.EnsureParticipant("owner2@example.test", "Owner Two", "example.test")
	require.NoError(t, f.f.Store.AddAccountIdentity(f.f.Source.ID, "owner2@example.test", "manual"))
	secondOwner, _, err := f.f.Store.CreatePersonFromParticipant(second)
	require.NoError(t, err)
	f.run(t)
	assert.Empty(t, f.values(t, false))
	require.NoError(t, f.f.Store.DeletePersonContext(t.Context(), secondOwner.ID, secondOwner.Revision))
	f.run(t)
	values = f.values(t, false)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, owner.ID, *values[0].Value.RecordID)
}

func TestIntroducedByDeletedSenderAndDeclaredReference(t *testing.T) {
	f := newIntroFixture(t)
	sam, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	f.run(t)
	require.Len(t, f.values(t, false), 1)
	require.NoError(t, f.f.Store.DeletePersonContext(t.Context(), sam.ID, sam.Revision))
	f.run(t)
	assert.Empty(t, f.values(t, false))
	assert.Len(t, f.values(t, true), 1)
	sam, _, err = f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.run(t)
	values := f.values(t, false)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, sam.ID, *values[0].Value.RecordID)
	recordType := "person"
	_, err = f.f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: &recordType, RecordID: &sam.ID}, Source: store.ProvenanceUser})
	require.NoError(t, err)
	assert.ErrorIs(t, f.f.Store.DeletePersonContext(t.Context(), sam.ID, sam.Revision), store.ErrPersonReferenced)
	ref := "First-Contact:message:123"
	_, err = f.f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: &recordType, RecordID: &sam.ID}, Source: store.ProvenanceSystem, SourceRef: &ref})
	require.NoError(t, err)
	assert.ErrorIs(t, f.f.Store.DeletePersonContext(t.Context(), sam.ID, sam.Revision), store.ErrPersonReferenced)
	values = f.values(t, false)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].SourceRef)
	assert.Equal(t, ref, *values[0].SourceRef)
}

func TestFirstContactBackfill(t *testing.T) {
	f := newIntroFixture(t)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	f.run(t)
	ctx := t.Context()
	_, err := f.f.Store.DB().ExecContext(ctx, `DELETE FROM person_first_contacts`)
	require.NoError(t, err)
	_, err = f.f.Store.DB().ExecContext(ctx, `DELETE FROM applied_migrations WHERE name = 'person_first_contacts_v1'`)
	require.NoError(t, err)
	require.NoError(t, f.f.Store.InitSchemaContext(ctx))
	var dirty sql.NullTime
	err = f.f.Store.DB().QueryRowContext(ctx, f.f.Store.Rebind(`SELECT dirty_at FROM person_first_contacts WHERE person_id = ?`), f.person.ID).Scan(&dirty)
	require.NoError(t, err)
	assert.True(t, dirty.Valid)
}

func TestIntroducedByMergeRetiresSelfReference(t *testing.T) {
	f := newIntroFixture(t)
	sender, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	f.run(t)
	survivor, err := f.f.Store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	sender, err = f.f.Store.GetPersonContext(t.Context(), sender.ID)
	require.NoError(t, err)
	_, err = f.f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: sender.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: sender.Revision, IdempotencyKey: "merge-introducer", Actor: "test"})
	require.NoError(t, err)
	f.run(t)
	assert.Empty(t, f.values(t, false))
}

func TestFirstContactReconciliationIsBounded(t *testing.T) {
	f := newIntroFixture(t)
	_, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	f.run(t)
	ctx := t.Context()
	_, err = f.f.Store.DB().ExecContext(ctx, `UPDATE person_first_contacts SET dirty_at = CURRENT_TIMESTAMP`)
	require.NoError(t, err)
	var dirtyCount int
	require.NoError(t, f.f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_first_contacts WHERE dirty_at IS NOT NULL`).Scan(&dirtyCount))
	require.GreaterOrEqual(t, dirtyCount, 2)
	bounded, err := activity.NewProjector(f.f.Store, activity.Options{Timezone: "UTC", BatchSize: 1, MaxBatches: 1, MaxDirectCounterparts: 25})
	require.NoError(t, err)
	_, err = bounded.RunOnce(ctx)
	require.ErrorIs(t, err, activity.ErrWorkRemaining)
	var remaining int
	require.NoError(t, f.f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_first_contacts WHERE dirty_at IS NOT NULL`).Scan(&remaining))
	assert.Positive(t, remaining)
	f.run(t)
	require.NoError(t, f.f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_first_contacts WHERE dirty_at IS NOT NULL`).Scan(&remaining))
	assert.Zero(t, remaining)
}

func TestIntroducedByClearSurvivesMergeAndExactSplit(t *testing.T) {
	f := newIntroFixture(t)
	_, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	f.run(t)
	values := f.values(t, false)
	require.Len(t, values, 1)
	_, err = f.f.Store.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &values[0].ID})
	require.NoError(t, err)
	other := f.f.EnsureParticipant("survivor@example.test", "Survivor", "example.test")
	survivor, _, err := f.f.Store.CreatePersonFromParticipant(other)
	require.NoError(t, err)
	absorbed, err := f.f.Store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	merged, err := f.f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "clear-merge", Actor: "test"})
	require.NoError(t, err)
	f.person = &merged.Person
	f.run(t)
	assert.Empty(t, f.values(t, false))
	current, err := f.f.Store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	split, err := f.f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: current.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{f.target}, ExpectedSourceRevision: current.Revision, IdempotencyKey: "clear-split", Actor: "test"})
	require.NoError(t, err)
	assert.True(t, split.ExactReversal)
	f.person = &split.NewPerson
	f.run(t)
	assert.Empty(t, f.values(t, false))
	assert.NotEmpty(t, f.values(t, true))
	t.Run("absorbed clear preserves survivor value", func(t *testing.T) {
		f := newIntroFixture(t)
		_, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
		require.NoError(t, err)
		other := f.f.EnsureParticipant("survivor@example.test", "Survivor", "example.test")
		survivor, _, err := f.f.Store.CreatePersonFromParticipant(other)
		require.NoError(t, err)
		f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), f.owner, f.target, other)
		f.run(t)
		values := f.values(t, false)
		require.Len(t, values, 1)
		_, err = f.f.Store.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &values[0].ID})
		require.NoError(t, err)
		survivorValues, err := f.f.Store.ListPersonAttributeValuesContext(t.Context(), survivor.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugIntroducedBy})
		require.NoError(t, err)
		require.Len(t, survivorValues, 1)
		survivor, err = f.f.Store.GetPersonContext(t.Context(), survivor.ID)
		require.NoError(t, err)
		absorbed, err := f.f.Store.GetPersonContext(t.Context(), f.person.ID)
		require.NoError(t, err)
		merged, err := f.f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "survivor-value", Actor: "test"})
		require.NoError(t, err)
		f.person = &merged.Person
		values = f.values(t, false)
		require.Len(t, values, 1)
		assert.Equal(t, survivorValues[0].ID, values[0].ID)
	})
}

func TestFirstContactLockOrderPostgres(t *testing.T) {
	if db := os.Getenv("MSGVAULT_TEST_DB"); !strings.HasPrefix(db, "postgres://") && !strings.HasPrefix(db, "postgresql://") {
		t.Skip("PostgreSQL advisory locks")
	}
	f := newIntroFixture(t)
	sender, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	f.run(t)
	ctx := t.Context()
	revisions, err := f.f.Store.ContactRevisionsContext(ctx)
	require.NoError(t, err)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		recordType := "person"
		for range 10 {
			write, err := f.f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: &recordType, RecordID: &sender.ID}, Source: store.ProvenanceUser})
			if err != nil {
				errs <- err
				return
			}
			_, err = f.f.Store.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &write.Value.ID})
			if err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 10 {
			if _, err := f.f.Store.DB().ExecContext(ctx, f.f.Store.Rebind(`UPDATE person_first_contacts SET dirty_at = CURRENT_TIMESTAMP WHERE person_id = ?`), f.person.ID); err != nil {
				errs <- err
				return
			}
			if _, err := f.f.Store.ReconcileFirstContactsContext(ctx, 10, revisions); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

func TestFirstContactStaleFenceLeavesDirty(t *testing.T) {
	for _, kind := range []string{"identity epoch changed", "queue not drained"} {
		t.Run(kind, func(t *testing.T) {
			f := newIntroFixture(t)
			f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
			f.run(t)
			ctx := t.Context()
			_, err := f.f.Store.DB().ExecContext(ctx, f.f.Store.Rebind(`UPDATE person_first_contacts SET dirty_at = CURRENT_TIMESTAMP WHERE person_id = ?`), f.person.ID)
			require.NoError(t, err)
			revisions, err := f.f.Store.ContactRevisionsContext(ctx)
			require.NoError(t, err)
			if kind == "identity epoch changed" {
				_, _, err = f.f.Store.CreatePersonFromParticipant(f.sender)
				require.NoError(t, err)
			} else {
				f.message(t, "pending", f.sender, time.Date(2023, 5, 1, 12, 0, 0, 0, time.UTC))
			}
			count, err := f.f.Store.ReconcileFirstContactsContext(ctx, 10, revisions)
			assert.ErrorAs(t, err, new(*store.ErrActivityProjectionStale))
			assert.Zero(t, count)
			var dirty sql.NullTime
			err = f.f.Store.DB().QueryRowContext(ctx, f.f.Store.Rebind(`SELECT dirty_at FROM person_first_contacts WHERE person_id = ?`), f.person.ID).Scan(&dirty)
			require.NoError(t, err)
			assert.True(t, dirty.Valid)
		})
	}
}

func TestIntroducedByBackstopReconcilesRecipientOnlyChange(t *testing.T) {
	f := newIntroFixture(t)
	_, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	recipients := []int64{f.owner, f.target}
	for i := range 5 {
		recipients = append(recipients, f.f.EnsureParticipant(fmt.Sprintf("extra%d@example.test", i), "Extra", "example.test"))
	}
	id := f.message(t, "intro-audience", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), recipients[:6]...)
	f.run(t)
	require.Len(t, f.values(t, false), 1)
	require.NoError(t, f.f.Store.ReplaceMessageRecipients(id, "to", recipients, nil))
	_, err = f.projector.RunBackstop(t.Context())
	require.NoError(t, err)
	assert.Empty(t, f.values(t, false))
}

func TestSubsetPreservesFirstContactState(t *testing.T) {
	for _, merge := range []bool{true, false} {
		name := "clear"
		if merge {
			name = "merge packet"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "source.db")
			st, err := store.OpenContext(ctx, path)
			require.NoError(t, err)
			require.NoError(t, st.InitSchemaContext(ctx))
			source, err := st.GetOrCreateSource("gmail", "owner@example.test")
			require.NoError(t, err)
			conv, err := st.EnsureConversation(ctx, source.ID, "thread", "Thread")
			require.NoError(t, err)
			fixture := &storetest.Fixture{T: t, Store: st, Source: source, ConvID: conv}
			require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.test", "manual"))
			owner := fixture.EnsureParticipant("owner@example.test", "Owner", "example.test")
			sender := fixture.EnsureParticipant("sam@example.test", "Sam", "example.test")
			target := fixture.EnsureParticipant("alex@example.test", "Alex", "example.test")
			other := fixture.EnsureParticipant("riley@example.test", "Riley", "example.test")
			person, _, err := st.CreatePersonFromParticipant(target)
			require.NoError(t, err)
			_, _, err = st.CreatePersonFromParticipant(sender)
			require.NoError(t, err)
			absorbed, _, err := st.CreatePersonFromParticipant(other)
			require.NoError(t, err)
			projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
			require.NoError(t, err)
			f := &introFixture{f: fixture, projector: projector, owner: owner, sender: sender, target: target, person: person}
			f.message(t, "intro", sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), owner, target, other)
			f.run(t)
			var projectedRows int
			err = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM person_first_contacts WHERE person_id IN (?, ?)`), person.ID, absorbed.ID).Scan(&projectedRows)
			require.NoError(t, err)
			require.Equal(t, 2, projectedRows)
			values := f.values(t, false)
			require.Len(t, values, 1)
			var mergeID int64
			if merge {
				person, err = st.GetPersonContext(ctx, person.ID)
				require.NoError(t, err)
				absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
				require.NoError(t, err)
				result, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: person.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: person.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "subset-first-contact", Actor: "test"})
				require.NoError(t, err)
				mergeID = result.Merge.ID
			} else {
				_, err = st.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &values[0].ID})
				require.NoError(t, err)
			}
			require.NoError(t, st.Close())
			dir := filepath.Join(t.TempDir(), "subset")
			_, err = store.CopySubsetWithOptions(ctx, path, dir, 10, store.CopySubsetOptions{IncludeIdentity: true, IncludeProfiles: true, IncludeAttributes: true, IncludeVCardResources: merge})
			require.NoError(t, err)
			dst, err := store.OpenContext(ctx, filepath.Join(dir, "msgvault.db"))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, dst.Close()) })
			if merge {
				_, err = dst.GetPersonMergeContext(ctx, mergeID)
				require.NoError(t, err)
			} else {
				p, err := activity.NewProjector(dst, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
				require.NoError(t, err)
				_, err = p.RunBackstop(ctx)
				require.NoError(t, err)
				values, err := dst.ListPersonAttributeValuesContext(ctx, person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugIntroducedBy})
				require.NoError(t, err)
				assert.Empty(t, values)
			}
		})
	}
}
