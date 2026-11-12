package store_test

import (
	"database/sql"
	"fmt"
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
func (f *introFixture) message(t *testing.T, name string, sender int64, at time.Time) int64 {
	t.Helper()
	msg := f.f.NewMessage().WithSourceMessageID(name).WithSentAt(at).Build()
	msg.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	id, err := f.f.Store.PersistMessage(&store.MessagePersistData{Message: msg})
	require.NoError(t, err)
	require.NoError(t, f.f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender"}))
	require.NoError(t, f.f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, f.target}, []string{"Owner", "Alex"}))
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
	f := newIntroFixture(t)
	sam, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	f.message(t, "intro", f.sender, at)
	f.run(t)
	values := f.values(t, false)
	require.Len(t, values, 1)
	_, err = f.f.Store.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &values[0].ID})
	require.NoError(t, err)
	other := f.f.EnsureParticipant("other@example.test", "Other", "example.test")
	otherPerson, _, err := f.f.Store.CreatePersonFromParticipant(other)
	require.NoError(t, err)
	f.message(t, "older", other, at.Add(-time.Hour))
	f.run(t)
	assert.Empty(t, f.values(t, false))
	recordType := "person"
	_, err = f.f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: &recordType, RecordID: &sam.ID}, Source: store.ProvenanceUser})
	require.NoError(t, err)
	f.message(t, "oldest", other, at.Add(-2*time.Hour))
	f.run(t)
	values = f.values(t, false)
	require.Len(t, values, 1)
	assert.Equal(t, store.ProvenanceUser, values[0].Source)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, sam.ID, *values[0].Value.RecordID)
	assert.NotEqual(t, otherPerson.ID, *values[0].Value.RecordID)
}
func TestIntroducedByDisabledBacklogAndSweepExclusion(t *testing.T) {
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
	catalog, err := f.f.Store.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	for _, target := range catalog.Targets {
		assert.NotEqual(t, store.AttributeUniversalIDIntroducedBy, target.Key)
	}
}

func TestIntroducedByOwnerOutreachAndRetirement(t *testing.T) {
	f := newIntroFixture(t)
	owner, _, err := f.f.Store.CreatePersonFromParticipant(f.owner)
	require.NoError(t, err)
	f.message(t, "outreach", f.owner, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
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
	require.NoError(t, f.f.Store.DeletePersonContext(t.Context(), owner.ID, owner.Revision))
	f.run(t)
	assert.Empty(t, f.values(t, false))
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
}

func TestFirstContactBackfillAndStaleFence(t *testing.T) {
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
	revisions, err := f.f.Store.ContactRevisionsContext(ctx)
	require.NoError(t, err)
	_, _, err = f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	count, err := f.f.Store.ReconcileFirstContactsContext(ctx, 10, revisions)
	assert.ErrorAs(t, err, new(*store.ErrActivityProjectionStale))
	assert.Zero(t, count)
	err = f.f.Store.DB().QueryRowContext(ctx, f.f.Store.Rebind(`SELECT dirty_at FROM person_first_contacts WHERE person_id = ?`), f.person.ID).Scan(&dirty)
	require.NoError(t, err)
	assert.True(t, dirty.Valid)
	f.run(t)
	assert.Len(t, f.values(t, false), 1)
}

func TestIntroducedByMergeRepointsAndRetiresSelfReference(t *testing.T) {
	for _, intoTarget := range []bool{false, true} {
		t.Run(fmt.Sprintf("into-target-%t", intoTarget), func(t *testing.T) {
			f := newIntroFixture(t)
			sender, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
			require.NoError(t, err)
			f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
			f.run(t)
			survivor := f.person
			if !intoTarget {
				participant := f.f.EnsureParticipant("merged@example.test", "Merged", "example.test")
				survivor, _, err = f.f.Store.CreatePersonFromParticipant(participant)
				require.NoError(t, err)
			}
			survivor, err = f.f.Store.GetPersonContext(t.Context(), survivor.ID)
			require.NoError(t, err)
			sender, err = f.f.Store.GetPersonContext(t.Context(), sender.ID)
			require.NoError(t, err)
			_, err = f.f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: sender.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: sender.Revision, IdempotencyKey: "merge-introducer", Actor: "test"})
			require.NoError(t, err)
			f.run(t)
			values := f.values(t, false)
			if intoTarget {
				assert.Empty(t, values)
				return
			}
			require.Len(t, values, 1)
			require.NotNil(t, values[0].Value.RecordID)
			assert.Equal(t, survivor.ID, *values[0].Value.RecordID)
		})
	}
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
}

func TestFirstContactLockOrderPostgres(t *testing.T) {
	f := newIntroFixture(t)
	if !f.f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory locks")
	}
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

func TestIntroducedByDeclaredValueAndClearRemainOwned(t *testing.T) {
	for _, source := range []store.Provenance{store.ProvenanceUser, store.ProvenanceVCardImport} {
		t.Run(string(source), func(t *testing.T) {
			f := newIntroFixture(t)
			sender, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
			require.NoError(t, err)
			manualParticipant := f.f.EnsureParticipant("manual@example.test", "Manual", "example.test")
			manual, _, err := f.f.Store.CreatePersonFromParticipant(manualParticipant)
			require.NoError(t, err)
			recordType := "person"
			write, err := f.f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: &recordType, RecordID: &manual.ID}, Source: source})
			require.NoError(t, err)
			f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
			f.run(t)
			values := f.values(t, false)
			require.Len(t, values, 1)
			assert.Equal(t, write.Value.ID, values[0].ID)
			require.NotNil(t, values[0].Value.RecordID)
			assert.NotEqual(t, sender.ID, *values[0].Value.RecordID)
			_, err = f.f.Store.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: f.person.ID, DefinitionSlug: store.AttributeSlugIntroducedBy, ExpectedValueID: &write.Value.ID})
			require.NoError(t, err)
			f.message(t, "older", f.sender, time.Date(2023, 5, 1, 12, 0, 0, 0, time.UTC))
			f.run(t)
			assert.Empty(t, f.values(t, false))
		})
	}
}

func TestFirstContactPendingQueueLeavesDirty(t *testing.T) {
	f := newIntroFixture(t)
	_, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	f.message(t, "intro", f.sender, time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	f.run(t)
	ctx := t.Context()
	_, err = f.f.Store.DB().ExecContext(ctx, f.f.Store.Rebind(`UPDATE person_first_contacts SET dirty_at = CURRENT_TIMESTAMP WHERE person_id = ?`), f.person.ID)
	require.NoError(t, err)
	f.message(t, "pending", f.sender, time.Date(2023, 5, 1, 12, 0, 0, 0, time.UTC))
	revisions, err := f.f.Store.ContactRevisionsContext(ctx)
	require.NoError(t, err)
	count, err := f.f.Store.ReconcileFirstContactsContext(ctx, 10, revisions)
	assert.ErrorAs(t, err, new(*store.ErrActivityProjectionStale))
	assert.Zero(t, count)
	var dirty sql.NullTime
	err = f.f.Store.DB().QueryRowContext(ctx, f.f.Store.Rebind(`SELECT dirty_at FROM person_first_contacts WHERE person_id = ?`), f.person.ID).Scan(&dirty)
	require.NoError(t, err)
	assert.True(t, dirty.Valid)
	f.run(t)
}

func TestIntroducedByFirstMessageBreaksTimestampTie(t *testing.T) {
	f := newIntroFixture(t)
	firstSender, _, err := f.f.Store.CreatePersonFromParticipant(f.sender)
	require.NoError(t, err)
	second := f.f.EnsureParticipant("second@example.test", "Second", "example.test")
	_, _, err = f.f.Store.CreatePersonFromParticipant(second)
	require.NoError(t, err)
	at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	first := f.message(t, "first", f.sender, at)
	secondID := f.message(t, "second", second, at)
	require.Less(t, first, secondID)
	f.run(t)
	values := f.values(t, false)
	require.Len(t, values, 1)
	require.NotNil(t, values[0].Value.RecordID)
	assert.Equal(t, firstSender.ID, *values[0].Value.RecordID)
}
