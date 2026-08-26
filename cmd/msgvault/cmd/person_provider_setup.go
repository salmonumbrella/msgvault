package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

const maxProviderCredentialBytes = 16 << 10

type personProviderSetupDeps struct {
	catalog     func(context.Context) ([]peoplesweep.ProviderSuggestion, error)
	negotiate   func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error)
	credentials peoplesweep.CredentialStore
	lookupEnv   peoplesweep.CredentialLookup
	isTerminal  func(uintptr) bool
	readMasked  func(*os.File, int) ([]byte, error)
}

type personProviderCreateCredentialStore interface {
	SaveNew(profileName string, credential peoplesweep.Credential) (bool, error)
}

type personProviderAddOptions struct {
	custom              bool
	protocol            string
	endpoint            string
	model               string
	auth                string
	credentialEnv       string
	apiKeyStdin         bool
	retentionPosture    string
	trainingPosture     string
	allowedSources      []string
	sourceSince         string
	sourceUntil         string
	allowSensitive      bool
	reasoningEffort     string
	reasoningMode       string
	requestTimeout      time.Duration
	acceptCatalogPrices bool
	confirmed           bool
}

func defaultPersonProviderSetupDeps() personProviderSetupDeps {
	return personProviderSetupDeps{
		catalog: func(ctx context.Context) ([]peoplesweep.ProviderSuggestion, error) {
			return peoplesweep.NewModelsDevClient(http.DefaultClient).Fetch(ctx)
		},
		negotiate: func(
			ctx context.Context,
			candidate peoplesweep.ProviderConfig,
			credential peoplesweep.Credential,
		) (peoplesweep.NegotiatedCapabilities, error) {
			registry, err := peoplesweep.NewDriverRegistry(
				http.DefaultClient,
				peoplesweep.NewCodexCommandStarter(),
				peoplesweep.NewReleasedCodexIsolationGate(),
			)
			if err != nil {
				return peoplesweep.NegotiatedCapabilities{}, err
			}
			return peoplesweep.NewCapabilityChecker(registry).Negotiate(ctx, candidate, credential)
		},
		lookupEnv:  os.LookupEnv,
		isTerminal: term.IsTerminal,
		readMasked: readBoundedMaskedCredential,
	}
}

func newPersonProviderAddCommand(deps personProviderCommandDeps) *cobra.Command {
	var options personProviderAddOptions
	command := &cobra.Command{
		Use:   "add <name>",
		Short: "Add and check a named people inference provider profile",
		Args:  exactPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return runPersonProviderAdd(command, deps, args[0], options)
		},
	}
	flags := command.Flags()
	flags.BoolVar(&options.custom, "custom", false, "Skip public catalog suggestions")
	flags.StringVar(&options.protocol, "protocol", "", "Explicit protocol identifier")
	flags.StringVar(&options.endpoint, "endpoint", "", "Explicit provider endpoint")
	flags.StringVar(&options.model, "model", "", "Explicit provider model identifier")
	flags.StringVar(&options.auth, "auth", "", "Explicit auth scheme")
	flags.StringVar(&options.credentialEnv, "credential-env", "", "Read only this environment variable")
	flags.BoolVar(&options.apiKeyStdin, "api-key-stdin", false, "Read the API key locally from standard input")
	flags.StringVar(&options.retentionPosture, "retention-posture", "", "Provider retention assertion")
	flags.StringVar(&options.trainingPosture, "training-posture", "", "Provider training assertion")
	flags.StringSliceVar(&options.allowedSources, "source", nil, "Allowed source class (repeatable)")
	flags.StringVar(&options.sourceSince, "source-since", "", "Earliest disclosed source date")
	flags.StringVar(&options.sourceUntil, "source-until", "", "Latest disclosed source date")
	flags.BoolVar(&options.allowSensitive, "allow-sensitive", false, "Allow sensitive text in provider packets")
	flags.StringVar(&options.reasoningEffort, "reasoning-effort", "", "Explicit reasoning effort")
	flags.StringVar(&options.reasoningMode, "reasoning-mode", "", "Explicit reasoning mode")
	flags.DurationVar(&options.requestTimeout, "request-timeout", time.Minute, "Provider request timeout")
	flags.BoolVar(&options.acceptCatalogPrices, "accept-catalog-prices", false,
		"Explicitly copy the exact matching catalog price hint into sweep budgets")
	flags.BoolVar(&options.confirmed, "yes", false, "Confirm the final provider and privacy values")
	return command
}

func runPersonProviderAdd(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	options personProviderAddOptions,
) error {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return err
	}
	if deps.readConfigFile == nil || deps.editConfigTables == nil || deps.restoreConfigFile == nil {
		return errors.New("people provider config editing is unavailable")
	}
	candidate, err := personProviderCandidate(options)
	if err != nil {
		return err
	}
	if err := validatePersonProviderAddOptions(candidate, options); err != nil {
		return err
	}
	configured := deps.config()
	if _, exists := configured.Providers[name]; exists {
		return fmt.Errorf("people provider profile %q already exists", name)
	}
	if err := validatePersonProviderCandidate(configured, name, candidate); err != nil {
		return err
	}
	printPersonProviderCandidate(command.OutOrStdout(), name, candidate)

	var suggestions []peoplesweep.ProviderSuggestion
	if !options.custom && deps.setup.catalog != nil {
		var err error
		suggestions, err = deps.setup.catalog(command.Context())
		if err != nil {
			_, _ = fmt.Fprintln(command.ErrOrStderr(),
				"models.dev suggestions are unavailable; explicit custom setup remains available.")
		} else {
			printPersonProviderSuggestions(command.OutOrStdout(), suggestions)
		}
	}
	before, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	configured, err = personProviderConfigFromSnapshot(deps, before)
	if err != nil {
		return err
	}
	if _, exists := configured.Providers[name]; exists {
		return fmt.Errorf("people provider profile %q already exists", name)
	}
	if err := validatePersonProviderCandidate(configured, name, candidate); err != nil {
		return err
	}
	var catalogPrices *peoplesweep.BudgetConfig
	if options.acceptCatalogPrices {
		catalogPrices, err = acceptedPersonProviderCatalogPrices(configured.Budgets, suggestions, candidate)
		if err != nil {
			return err
		}
	}

	credential, stored, err := readPersonProviderCredential(command, deps.setup, name, candidate, options)
	if err != nil {
		return err
	}
	if deps.setup.negotiate == nil {
		return errors.New("people provider capability negotiation is unavailable")
	}
	capabilities, err := deps.setup.negotiate(command.Context(), candidate, credential)
	if err != nil {
		return err
	}
	candidate.OutputMode = capabilities.OutputMode
	candidate.TokenLimitParameter = capabilities.TokenLimitParameter
	candidate.ReasoningEffort = capabilities.ReasoningEffort
	candidate.ReasoningMode = capabilities.ReasoningMode
	candidate.DriverVersion = capabilities.DriverVersion
	if err := validatePersonProviderCandidate(configured, name, candidate); err != nil {
		return err
	}

	createdCredential := false
	if stored {
		if deps.setup.credentials == nil {
			return errors.New("people provider credential store is unavailable")
		}
		createStore, ok := deps.setup.credentials.(personProviderCreateCredentialStore)
		if !ok {
			return errors.New("people provider credential store does not support create-only publication")
		}
		createdCredential, err = createStore.SaveNew(name, credential)
		if err != nil {
			return err
		}
		if !createdCredential {
			return fmt.Errorf("people provider credential %q already exists", name)
		}
	}

	edits := []config.TableEdit{
		{Path: []string{"people", "sweep"}, Values: map[string]any{"provider": name}},
		{Path: []string{"people", "sweep", "providers", name}, Values: personProviderTableValues(candidate), InsertOnly: true},
	}
	if catalogPrices != nil {
		edits = append(edits, config.TableEdit{
			Path: []string{"people", "sweep", "budgets"},
			Values: map[string]any{
				"input_cost_microusd_per_million_tokens":  catalogPrices.InputCostMicroUSDPerMillionTokens,
				"output_cost_microusd_per_million_tokens": catalogPrices.OutputCostMicroUSDPerMillionTokens,
			},
		})
	}
	after, err := deps.editConfigTables(before.ETag, edits)
	if err != nil {
		if errors.Is(err, config.ErrConfigChanged) {
			return rollbackUncertainPersonProviderAdd(err, deps, before, after, name, createdCredential)
		}
		return rollbackNewPersonProviderCredential(err, deps.setup.credentials, name, createdCredential)
	}
	checkedConfig, err := personProviderConfigFromSnapshot(deps, after)
	if err == nil {
		checkedDeps := deps
		checkedDeps.config = func() peoplesweep.Config { return checkedConfig }
		err = executeSavedPersonProviderCheck(command, checkedDeps, name)
	}
	if err != nil {
		rollbackErr := rollbackPersonProviderAdd(deps, before, after, name, createdCredential)
		return errors.Join(err, rollbackErr)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Added and checked people provider profile %q.\n", name)
	return nil
}

func personProviderCandidate(options personProviderAddOptions) (peoplesweep.ProviderConfig, error) {
	if options.protocol == "" || options.model == "" || options.auth == "" ||
		options.retentionPosture == "" || options.trainingPosture == "" ||
		len(options.allowedSources) == 0 || options.sourceSince == "" {
		return peoplesweep.ProviderConfig{}, errors.New("protocol, model, auth, retention, training, source, and source-since are required")
	}
	candidate := peoplesweep.ProviderConfig{
		Protocol: peoplesweep.Protocol(options.protocol), Endpoint: options.endpoint,
		Model: options.model, Auth: peoplesweep.AuthScheme(options.auth),
		RetentionPosture: options.retentionPosture, TrainingPosture: options.trainingPosture,
		SourceSince: options.sourceSince, SourceUntil: options.sourceUntil,
		AllowSensitive: options.allowSensitive, ReasoningEffort: options.reasoningEffort,
		ReasoningMode: options.reasoningMode, RequestTimeout: options.requestTimeout,
	}
	for _, source := range options.allowedSources {
		candidate.AllowedSources = append(candidate.AllowedSources, peoplesweep.SourceClass(source))
	}
	if candidate.Auth == peoplesweep.AuthNone {
		candidate.Credential = peoplesweep.CredentialNone
	} else if options.credentialEnv != "" {
		candidate.Credential = peoplesweep.CredentialEnv
		candidate.CredentialEnv = options.credentialEnv
	} else {
		candidate.Credential = peoplesweep.CredentialStored
	}
	if candidate.Protocol == peoplesweep.ProtocolOpenAIChat {
		candidate.OutputMode = peoplesweep.OutputModeNativeJSONSchema
		candidate.TokenLimitParameter = "max_completion_tokens"
	} else {
		candidate.OutputMode = peoplesweep.OutputModeNativeJSONSchema
	}
	return candidate, nil
}

func validatePersonProviderAddOptions(
	candidate peoplesweep.ProviderConfig,
	options personProviderAddOptions,
) error {
	if !options.confirmed {
		return errors.New("people provider add requires --yes after reviewing the final values")
	}
	if options.custom && options.acceptCatalogPrices {
		return errors.New("--custom cannot be combined with --accept-catalog-prices")
	}
	if options.apiKeyStdin && options.credentialEnv != "" {
		return errors.New("--api-key-stdin and --credential-env are mutually exclusive")
	}
	if candidate.Credential == peoplesweep.CredentialNone &&
		(options.apiKeyStdin || options.credentialEnv != "") {
		return errors.New("auth=none cannot accept a credential")
	}
	return nil
}

func personProviderConfigFromSnapshot(
	deps personProviderCommandDeps,
	snapshot config.ConfigFile,
) (peoplesweep.Config, error) {
	homeDir := ""
	if deps.configHomeDir != nil {
		homeDir = deps.configHomeDir()
	}
	loaded, err := config.LoadConfigFile(snapshot, homeDir)
	if err != nil {
		return peoplesweep.Config{}, err
	}
	return loaded.People.Sweep, nil
}

func readPersonProviderCredential(
	command *cobra.Command,
	setup personProviderSetupDeps,
	name string,
	candidate peoplesweep.ProviderConfig,
	options personProviderAddOptions,
) (peoplesweep.Credential, bool, error) {
	if candidate.Credential == peoplesweep.CredentialNone {
		if options.apiKeyStdin || options.credentialEnv != "" {
			return peoplesweep.Credential{}, false, errors.New("auth=none cannot accept a credential")
		}
		return peoplesweep.NewCredential(peoplesweep.AuthNone, ""), false, nil
	}
	if options.apiKeyStdin && options.credentialEnv != "" {
		return peoplesweep.Credential{}, false, errors.New("--api-key-stdin and --credential-env are mutually exclusive")
	}
	if candidate.Credential == peoplesweep.CredentialEnv {
		if setup.lookupEnv == nil {
			return peoplesweep.Credential{}, false, errors.New("people provider environment lookup is unavailable")
		}
		value, ok := setup.lookupEnv(options.credentialEnv)
		if !ok || value == "" {
			return peoplesweep.Credential{}, false,
				fmt.Errorf("people provider credential environment variable %s is not set", options.credentialEnv)
		}
		return peoplesweep.NewCredential(candidate.Auth, value), false, nil
	}
	var raw []byte
	var err error
	if options.apiKeyStdin {
		raw, err = readProviderCredentialLine(command.InOrStdin())
	} else {
		file, ok := command.InOrStdin().(*os.File)
		if !ok || setup.isTerminal == nil || !setup.isTerminal(file.Fd()) || setup.readMasked == nil {
			return peoplesweep.Credential{}, false,
				errors.New("a masked terminal is required; use --api-key-stdin for non-interactive setup")
		}
		_, _ = fmt.Fprintf(command.ErrOrStderr(), "API key for %s: ", name)
		raw, err = setup.readMasked(file, maxProviderCredentialBytes)
		_, _ = fmt.Fprintln(command.ErrOrStderr())
	}
	if err != nil {
		return peoplesweep.Credential{}, false, errors.New("read people provider credential")
	}
	if len(raw) == 0 || len(raw) > maxProviderCredentialBytes {
		return peoplesweep.Credential{}, false, errors.New("people provider credential is empty or too large")
	}
	return peoplesweep.NewCredential(candidate.Auth, string(raw)), true, nil
}

func readBoundedMaskedCredential(file *os.File, limit int) ([]byte, error) {
	return readBoundedMaskedCredentialWithTerminal(
		file, file.Fd(), limit, term.MakeRaw, term.Restore,
	)
}

func readBoundedMaskedCredentialWithTerminal(
	reader io.Reader,
	fd uintptr,
	limit int,
	makeRaw func(uintptr) (*term.State, error),
	restore func(uintptr, *term.State) error,
) (credential []byte, resultErr error) {
	state, err := makeRaw(fd)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := restore(fd, state); err != nil {
			clear(credential)
			credential = nil
			resultErr = errors.Join(resultErr, fmt.Errorf("restore credential terminal: %w", err))
		}
	}()
	return readBoundedMaskedCredentialInput(reader, limit)
}

func readBoundedMaskedCredentialInput(reader io.Reader, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("credential input limit is invalid")
	}
	credential := make([]byte, 0, min(limit, 128))
	tooLarge := false
	var input [1]byte
	for {
		read, err := reader.Read(input[:])
		if read > 0 {
			switch input[0] {
			case '\r', '\n', 0x04:
				if tooLarge {
					clear(credential)
					return nil, errors.New("credential is too large")
				}
				return credential, nil
			case 0x03:
				clear(credential)
				return nil, errors.New("credential entry canceled")
			case 0x08, 0x7f:
				if !tooLarge && len(credential) > 0 {
					credential = credential[:len(credential)-1]
				}
			default:
				if len(credential) == limit {
					tooLarge = true
				} else if !tooLarge {
					credential = append(credential, input[0])
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && !tooLarge {
				return credential, nil
			}
			clear(credential)
			if tooLarge {
				return nil, errors.New("credential is too large")
			}
			return nil, err
		}
		if read == 0 {
			clear(credential)
			return nil, io.ErrNoProgress
		}
	}
}

func readProviderCredentialLine(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxProviderCredentialBytes+2))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxProviderCredentialBytes+1 {
		return nil, errors.New("credential is too large")
	}
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
	}
	if len(raw) > maxProviderCredentialBytes {
		return nil, errors.New("credential is too large")
	}
	for _, value := range raw {
		if value == '\r' || value == '\n' {
			return nil, errors.New("credential input contains trailing data")
		}
	}
	return raw, nil
}

func validatePersonProviderCandidate(
	configured peoplesweep.Config,
	name string,
	candidate peoplesweep.ProviderConfig,
) error {
	configured.Enabled = true
	configured.Provider = peoplesweep.ProviderSelection{Name: name}
	providers := make(map[string]peoplesweep.ProviderConfig, len(configured.Providers)+1)
	for existingName, existing := range configured.Providers {
		providers[existingName] = existing
	}
	providers[name] = candidate
	configured.Providers = providers
	configured.ApplyDefaults()
	_, err := configured.Profile()
	return err
}

func personProviderTableValues(provider peoplesweep.ProviderConfig) map[string]any {
	sources := make([]string, len(provider.AllowedSources))
	for index, source := range provider.AllowedSources {
		sources[index] = string(source)
	}
	values := map[string]any{
		"protocol": provider.Protocol, "endpoint": provider.Endpoint, "model": provider.Model,
		"auth": provider.Auth, "credential": provider.Credential,
		"output_mode": provider.OutputMode, "token_limit_parameter": provider.TokenLimitParameter,
		"retention_posture": provider.RetentionPosture, "training_posture": provider.TrainingPosture,
		"allowed_sources": sources, "source_since": provider.SourceSince,
		"allow_sensitive": provider.AllowSensitive, "request_timeout": provider.RequestTimeout,
	}
	if provider.CredentialEnv != "" {
		values["credential_env"] = provider.CredentialEnv
	}
	if provider.SourceUntil != "" {
		values["source_until"] = provider.SourceUntil
	}
	if provider.ReasoningEffort != "" {
		values["reasoning_effort"] = provider.ReasoningEffort
	}
	if provider.ReasoningMode != "" {
		values["reasoning_mode"] = provider.ReasoningMode
	}
	return values
}

func acceptedPersonProviderCatalogPrices(
	current peoplesweep.BudgetConfig,
	suggestions []peoplesweep.ProviderSuggestion,
	candidate peoplesweep.ProviderConfig,
) (*peoplesweep.BudgetConfig, error) {
	type pair struct{ input, output *int64 }
	var matches []pair
	for _, provider := range suggestions {
		if provider.Endpoint != candidate.Endpoint {
			continue
		}
		for _, model := range provider.Models {
			if model.ID == candidate.Model {
				matches = append(matches, pair{model.InputCostMicroUSDPerMillionTokens,
					model.OutputCostMicroUSDPerMillionTokens})
			}
		}
	}
	if len(matches) != 1 || matches[0].input == nil || matches[0].output == nil {
		return nil, errors.New("exactly one complete catalog price suggestion must match the explicit endpoint and model")
	}
	current.InputCostMicroUSDPerMillionTokens = *matches[0].input
	current.OutputCostMicroUSDPerMillionTokens = *matches[0].output
	return &current, nil
}

func printPersonProviderSuggestions(w io.Writer, suggestions []peoplesweep.ProviderSuggestion) {
	for _, provider := range suggestions {
		_, _ = fmt.Fprintf(w, "Suggestion: %s (%s) endpoint=%s\n", provider.Name, provider.ID, provider.Endpoint)
		for _, model := range provider.Models {
			_, _ = fmt.Fprintf(w, "  model hint: %s (%s)\n", model.Name, model.ID)
		}
	}
}

func printPersonProviderCandidate(w io.Writer, name string, candidate peoplesweep.ProviderConfig) {
	_, _ = fmt.Fprintf(w,
		"Final provider %q: endpoint=%s protocol=%s model=%s auth=%s credential=%s retention=%s training=%s sources=%s sensitive=%t\n",
		name, candidate.Endpoint, candidate.Protocol, candidate.Model, candidate.Auth,
		candidate.Credential, candidate.RetentionPosture, candidate.TrainingPosture,
		strings.Join(sourceStrings(candidate.AllowedSources), ","), candidate.AllowSensitive)
}

func sourceStrings(sources []peoplesweep.SourceClass) []string {
	result := make([]string, len(sources))
	for index, source := range sources {
		result[index] = string(source)
	}
	return result
}

func executeSavedPersonProviderCheck(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
) error {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return err
	}
	directStore := deps.isDaemonSubprocess != nil && deps.isDaemonSubprocess()
	if !directStore && deps.providerStoreOwnedByDaemon != nil {
		owned, err := deps.providerStoreOwnedByDaemon(command.Context())
		if err != nil {
			return err
		}
		directStore = !owned
	}
	if directStore {
		return runPersonProviderCheck(command, deps, name, false)
	}
	return proxySavedPersonProviderOperation(command, deps, "check", name)
}

func proxySavedPersonProviderRevoke(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
) error {
	return proxySavedPersonProviderOperation(command, deps, "revoke", name)
}

func proxySavedPersonProviderOperation(
	command *cobra.Command,
	deps personProviderCommandDeps,
	operation string,
	name string,
) error {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return err
	}
	if deps.proxy == nil {
		return errors.New("people provider daemon proxy is unavailable")
	}
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	provider := &cobra.Command{Use: "provider"}
	leaf := &cobra.Command{Use: operation}
	provider.AddCommand(leaf)
	person.AddCommand(provider)
	root.AddCommand(person)
	leaf.SetOut(command.OutOrStdout())
	leaf.SetErr(command.ErrOrStderr())
	return deps.proxy(leaf, []string{name}, nil)
}

func rollbackNewPersonProviderCredential(
	cause error,
	credentials peoplesweep.CredentialStore,
	name string,
	created bool,
) error {
	if !created || credentials == nil {
		return cause
	}
	if err := credentials.Delete(name); err != nil {
		return errors.Join(cause, fmt.Errorf("delete newly created people provider credential: %w", err))
	}
	return cause
}

func rollbackUncertainPersonProviderAdd(
	cause error,
	deps personProviderCommandDeps,
	before, expected config.ConfigFile,
	name string,
	createdCredential bool,
) error {
	current, err := deps.readConfigFile()
	if err != nil {
		cleanupErr := rollbackNewPersonProviderCredential(nil, deps.setup.credentials, name, createdCredential)
		return errors.Join(cause, fmt.Errorf("inspect uncertain people provider config publication: %w", err), cleanupErr)
	}
	if config.SameConfigFileVersion(current, expected) {
		return errors.Join(cause, rollbackPersonProviderAdd(deps, before, current, name, createdCredential))
	}
	cleanupErr := rollbackNewPersonProviderCredential(nil, deps.setup.credentials, name, createdCredential)
	return errors.Join(cause,
		errors.New("people provider config publication is uncertain and the current version was preserved"),
		cleanupErr)
}

func rollbackPersonProviderAdd(
	deps personProviderCommandDeps,
	before, after config.ConfigFile,
	name string,
	createdCredential bool,
) error {
	var rollbackErr error
	if _, err := deps.restoreConfigFile(after.ETag, before); err != nil {
		rollbackErr = fmt.Errorf("restore people provider config: %w", err)
	}
	if createdCredential && deps.setup.credentials != nil {
		if err := deps.setup.credentials.Delete(name); err != nil {
			rollbackErr = errors.Join(rollbackErr,
				fmt.Errorf("delete newly created people provider credential: %w", err))
		}
	}
	return rollbackErr
}
