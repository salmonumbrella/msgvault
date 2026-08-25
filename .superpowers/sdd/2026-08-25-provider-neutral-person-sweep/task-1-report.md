---
last_edited: 2026-08-25
---

# Task 1 report: named provider configuration and legacy loading

## Implementation summary

- Replaced the single people-sweep provider table with an active `ProviderSelection` and named `Providers` map.
- Added typed protocol, output-mode, auth-scheme, and credential-source values plus the provider-profile fields required by later tasks.
- Added `ActiveProviderConfig()` value-based resolution and updated runtime factories, runner paths, CLI forwarding, API forwarding, and Codex helpers to resolve the active provider without retaining map-entry pointers.
- Preserved legacy `[people.sweep.provider]` loading as the named `default` profile, including OpenAI-compatible, anonymous-loopback, and Codex defaults. Mixed legacy/new shapes and unknown active names fail closed.
- Added protocol-specific validation, normalized reasoning validation, canonical policy encoding, and fingerprint coverage for protocol capabilities, credential references, privacy declarations, driver version, renderer policy, and program fingerprint. Request timeout, executable path, and credential secret values remain outside the policy fingerprint.
- Adapted person-inference profile persistence to the new profile fields while retaining semantic JSON comparison and canonical reconstruction on reads.

## Files changed

Production:

- `internal/peoplesweep/config.go`
- `internal/peoplesweep/transport_factory.go`
- `internal/peoplesweep/openai_compatible.go`
- `internal/peoplesweep/codex_app_server.go`
- `internal/peoplesweep/runner.go`
- `internal/peoplesweep/worker.go`
- `internal/config/config.go`
- `cmd/msgvault/cmd/person_provider.go`
- `cmd/msgvault/cmd/person_sweep.go`
- `cmd/msgvault/cmd/serve_people_sweep.go`
- `internal/api/cli_handlers.go`
- `internal/store/person_inference_consent.go`

Tests:

- `internal/peoplesweep/config_test.go`
- `internal/peoplesweep/transport_factory_test.go`
- `internal/peoplesweep/openai_compatible_test.go`
- `internal/peoplesweep/openai_end_to_end_test.go`
- `internal/peoplesweep/codex_app_server_test.go`
- `internal/peoplesweep/codex_isolation_test.go`
- `internal/peoplesweep/codex_isolation_integration_test.go`
- `internal/peoplesweep/runner_test.go`
- `internal/peoplesweep/worker_test.go`
- `internal/peoplesweep/program_test.go`
- `internal/peoplesweep/evaluation_test.go`
- `internal/config/people_sweep_test.go`
- `cmd/msgvault/cmd/person_provider_test.go`
- `cmd/msgvault/cmd/person_provider_daemon_test.go`
- `cmd/msgvault/cmd/person_provider_routing_test.go`
- `cmd/msgvault/cmd/person_sweep_test.go`
- `cmd/msgvault/cmd/serve_people_sweep_test.go`
- `internal/api/cli_allowlist_person_provider_test.go`
- `internal/store/person_inference_consent_test.go`
- `internal/store/person_sweep_worker_test.go`
- `internal/store/person_sweep_parity_test.go`

## Self-review

- Confirmed all five required test names exist and exercise production decoding, migration, validation, and fingerprint behavior.
- Confirmed `ProviderSelection.MarshalTOML` emits only the quoted active name.
- Confirmed legacy migration occurs only without a named-provider map; mixed shapes retain enough state for validation to reject them.
- Confirmed every direct people-sweep `Config.Provider` field access in the task surfaces was replaced with value-based active-provider resolution or test-only map mutation helpers.
- Confirmed HTTP validation allows authenticated loopback profiles, permits unauthenticated access only on loopback, and still rejects remote plain HTTP.
- Confirmed provider policy JSON contains no timeout, executable path, or secret value.
- Confirmed stored policy reads rebuild canonical bytes after semantic JSON comparison rather than weakening `ProviderProfile.Validate`.
- `git diff --check`, `go fmt ./...`, and `go vet -tags "fts5 sqlite_vec" ./...` completed cleanly.

## RED

Command:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/config -run 'TestConfig(LoadsNamed|MigratesLegacy|RejectsMixed|RejectsMissing)|TestProviderFingerprintIncludesProtocol' -count=1
```

Output:

```text
# go.kenn.io/msgvault/internal/peoplesweep_test [go.kenn.io/msgvault/internal/peoplesweep.test]
internal/peoplesweep/config_test.go:186:25: undefined: peoplesweep.ProviderSelection
internal/peoplesweep/config_test.go:189:5: unknown field Protocol in struct literal of type peoplesweep.ProviderConfig
internal/peoplesweep/config_test.go:189:27: undefined: peoplesweep.ProtocolOpenAIChat
internal/peoplesweep/config_test.go:190:24: unknown field Auth in struct literal of type peoplesweep.ProviderConfig
internal/peoplesweep/config_test.go:190:42: undefined: peoplesweep.AuthBearer
internal/peoplesweep/config_test.go:191:5: unknown field Credential in struct literal of type peoplesweep.ProviderConfig
internal/peoplesweep/config_test.go:191:29: undefined: peoplesweep.CredentialEnv
internal/peoplesweep/config_test.go:191:44: unknown field CredentialEnv in struct literal of type peoplesweep.ProviderConfig
internal/peoplesweep/config_test.go:192:5: unknown field OutputMode in struct literal of type peoplesweep.ProviderConfig
internal/peoplesweep/config_test.go:192:29: undefined: peoplesweep.OutputModeJSONObject
internal/peoplesweep/config_test.go:192:5: too many errors
FAIL	go.kenn.io/msgvault/internal/peoplesweep [build failed]
# go.kenn.io/msgvault/internal/config [go.kenn.io/msgvault/internal/config.test]
internal/config/people_sweep_test.go:205:45: loaded.People.Sweep.ActiveProviderConfig undefined (type peoplesweep.Config has no field or method ActiveProviderConfig)
internal/config/people_sweep_test.go:208:30: undefined: peoplesweep.ProtocolOpenAIChat
internal/config/people_sweep_test.go:211:30: undefined: peoplesweep.AuthBearer
internal/config/people_sweep_test.go:212:30: undefined: peoplesweep.CredentialEnv
internal/config/people_sweep_test.go:214:30: undefined: peoplesweep.OutputModeJSONObject
internal/config/people_sweep_test.go:238:45: loaded.People.Sweep.ActiveProviderConfig undefined (type peoplesweep.Config has no field or method ActiveProviderConfig)
internal/config/people_sweep_test.go:241:58: loaded.People.Sweep.Provider.Name undefined (type peoplesweep.ProviderConfig has no field or method Name)
internal/config/people_sweep_test.go:242:36: loaded.People.Sweep.Providers undefined (type peoplesweep.Config has no field or method Providers)
internal/config/people_sweep_test.go:243:30: undefined: peoplesweep.ProtocolOpenAIChat
internal/config/people_sweep_test.go:244:30: undefined: peoplesweep.AuthBearer
internal/config/people_sweep_test.go:244:30: too many errors
FAIL	go.kenn.io/msgvault/internal/config [build failed]
FAIL
```

This failed for the intended reason: the old configuration exposed one provider table and had none of the named-profile or typed-capability API required by the tests.

## GREEN

Command:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/config -run 'TestConfig(LoadsNamed|MigratesLegacy|RejectsMixed|RejectsMissing)|TestProviderFingerprintIncludesProtocol' -count=1
```

Output:

```text
ok  	go.kenn.io/msgvault/internal/peoplesweep	0.028s
ok  	go.kenn.io/msgvault/internal/config	0.027s
```

Additional successful verification:

```text
go test -tags "fts5 sqlite_vec" ./internal/peoplesweep ./internal/config -count=1
ok  	go.kenn.io/msgvault/internal/peoplesweep	19.607s
ok  	go.kenn.io/msgvault/internal/config	4.559s

go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/api -run 'Test(PersonProvider|PersonSweep|CLIRunEnvAllowed|CLIAllowlist|ProductionPersonSweep|ServePeopleSweep)' -count=1
ok  	go.kenn.io/msgvault/cmd/msgvault/cmd	5.837s
ok  	go.kenn.io/msgvault/internal/api	0.021s

go test -tags "fts5 sqlite_vec" ./internal/store -run 'TestPersonInference|TestPersonSweepWorkerFilteredNoText|TestPersonSweepSQLitePostgresParity' -count=1
ok  	go.kenn.io/msgvault/internal/store	1.915s

go vet -tags "fts5 sqlite_vec" ./...
[exit 0]
```

## Concerns

The required aggregate command did not complete successfully in this environment. It hit the existing 10-minute package timeout in `cmd/msgvault/cmd` during slow SQLite schema initialization; the aggregate run completed `internal/peoplesweep`, `internal/config`, and `internal/api` successfully before `cmd/msgvault/cmd` timed out. Running the shown changed-surface command passed, and the specific test observed in the aggregate timeout (`TestSetupVectorFeatures_SelectsRunnerByAPIFormat`) passed alone in 9.917s. No Task 1 failure was reported by the aggregate run, but it must not be described as passing.
