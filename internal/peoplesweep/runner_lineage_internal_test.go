package peoplesweep

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lineageProfileMutatingDriver struct {
	responses []DriverResponse
	seen      []ProviderProfile
	calls     int
}

func (d *lineageProfileMutatingDriver) Prepare(
	_ ProviderProfile,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	wire, err := json.Marshal(request)
	if err != nil {
		return PreparedStructuredRequest{}, err
	}
	return NewPreparedStructuredRequest(request, wire)
}

func (d *lineageProfileMutatingDriver) GeneratePrepared(
	_ context.Context,
	profile ProviderProfile,
	_ Credential,
	_ PreparedStructuredRequest,
) (DriverResponse, error) {
	snapshot := profile
	snapshot.AllowedSources = slices.Clone(profile.AllowedSources)
	snapshot.DisclosedPacketFields = slices.Clone(profile.DisclosedPacketFields)
	snapshot.PolicyJSON = slices.Clone(profile.PolicyJSON)
	d.seen = append(d.seen, snapshot)
	if d.calls == 0 {
		profile.AllowedSources[0] = SourceDocumentText
		profile.DisclosedPacketFields[0] = "mutated"
		profile.PolicyJSON[0] ^= 0xff
	}
	response := d.responses[d.calls]
	d.calls++
	return response, nil
}

func internalRunnerLineageFixture(
	t *testing.T,
	responses ...DriverResponse,
) (*Runner, *workerRepairDriver, PreparedStructuredRequest) {
	t.Helper()
	config, _ := workerTestConfig(t)
	driver := &workerRepairDriver{responses: responses}
	runner := newWorkerRepairRunner(t, config, driver)
	primary, err := runner.PrepareStructured(t.Context(), internalRunnerLineageRequest())
	require.NoError(t, err)
	return runner, driver, primary
}

func internalRunnerLineageRequest() StructuredRequest {
	return StructuredRequest{
		ProgramID: "lineage-test", ProgramVersion: "1",
		Sources: []SourceDescriptor{{
			Class: SourceConversationText, ObservedOn: "2025-08-20",
		}},
		InputText: `{"input":true}`, SchemaName: "lineage_test",
		JSONSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"ok":{"type":"boolean","const":true}},
			"required":["ok"],
			"additionalProperties":false
		}`),
		MaxOutputTokens: 32,
	}
}

func clonePreparedLineageFixture(prepared PreparedStructuredRequest) PreparedStructuredRequest {
	prepared.request = cloneStructuredRequest(prepared.request)
	prepared.wireRequest = append([]byte(nil), prepared.wireRequest...)
	return prepared
}

func TestRunnerExecutionSessionSnapshotsPreparedPrimaryBytes(t *testing.T) {
	runner, driver, primary := internalRunnerLineageFixture(t, DriverResponse{
		CandidateJSON:   json.RawMessage(`{"ok":true}`),
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
	})
	pristine := clonePreparedLineageFixture(primary)
	session, err := runner.BeginStructuredExecution(t.Context(), primary)
	require.NoError(t, err)

	primary.wireRequest[0] ^= 0xff
	primary.request.JSONSchema[0] ^= 0xff
	_, err = session.PrimaryCall(primary)
	require.ErrorContains(t, err, "bound primary")
	call, err := session.PrimaryCall(pristine)
	require.NoError(t, err)
	pristine.wireRequest[0] ^= 0xff
	pristine.request.JSONSchema[0] ^= 0xff
	started := 0
	_, err = call.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, started)
	assert.Equal(t, 1, driver.calls)
}

func TestRunnerExecutionSessionSnapshotsPreparedRepairBytes(t *testing.T) {
	runner, driver, primary := internalRunnerLineageFixture(t,
		DriverResponse{CandidateJSON: json.RawMessage(`{"ok":false}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"},
		DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"},
	)
	session, err := runner.BeginStructuredExecution(t.Context(), primary)
	require.NoError(t, err)
	primaryCall, err := session.PrimaryCall(primary)
	require.NoError(t, err)
	_, err = primaryCall.Execute(t.Context(), func(context.Context) error { return nil })
	var failure *ValidationFailure
	require.ErrorAs(t, err, &failure)
	repair, err := session.PrepareRepair(*failure)
	require.NoError(t, err)
	pristine := clonePreparedLineageFixture(repair)

	repair.wireRequest[0] ^= 0xff
	repair.request.JSONSchema[0] ^= 0xff
	_, err = session.RepairCall(repair)
	require.ErrorContains(t, err, "bound repair")
	repairCall, err := session.RepairCall(pristine)
	require.NoError(t, err)
	pristine.wireRequest[0] ^= 0xff
	pristine.request.JSONSchema[0] ^= 0xff
	started := 0
	_, err = repairCall.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, started)
	assert.Equal(t, 2, driver.calls)
}

func TestRunnerExecutionSessionSnapshotsProfileSlicesAcrossRepair(t *testing.T) {
	config, _ := workerTestConfig(t)
	driver := &lineageProfileMutatingDriver{responses: []DriverResponse{
		{CandidateJSON: json.RawMessage(`{"ok":false}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"},
		{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"},
	}}
	runner, err := NewRunner(config, workerRepairAuthority{},
		NewTestDriverRegistry(ProtocolOpenAIChat, driver),
		NewCredentialResolver(nil, func(string) (string, bool) { return "test-key", true }))
	require.NoError(t, err)
	primary, err := runner.PrepareStructured(t.Context(), internalRunnerLineageRequest())
	require.NoError(t, err)
	session, err := runner.BeginStructuredExecution(t.Context(), primary)
	require.NoError(t, err)
	primaryCall, err := session.PrimaryCall(primary)
	require.NoError(t, err)
	_, err = primaryCall.Execute(t.Context(), func(context.Context) error { return nil })
	var failure *ValidationFailure
	require.ErrorAs(t, err, &failure)
	repair, err := session.PrepareRepair(*failure)
	require.NoError(t, err)
	repairCall, err := session.RepairCall(repair)
	require.NoError(t, err)
	_, err = repairCall.Execute(t.Context(), func(context.Context) error { return nil })
	require.NoError(t, err)

	require.Len(t, driver.seen, 2)
	assert.Equal(t, driver.seen[0], driver.seen[1])
}
