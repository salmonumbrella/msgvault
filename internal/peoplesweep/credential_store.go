package peoplesweep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

var (
	credentialProfileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	ErrCredentialNotFound        = errors.New("people provider credential not found")
)

const credentialNamespace = "people-providers"

// Credential carries an authentication scheme and an opaque secret. The
// pointer-backed secret prevents fmt's value-%p special case from inspecting
// the underlying string when it bypasses Formatter.
type Credential struct {
	Scheme AuthScheme
	secret *credentialSecret
}

type credentialSecret struct {
	value string
}

// NewCredential constructs an opaque provider credential.
func NewCredential(scheme AuthScheme, value string) Credential {
	return Credential{Scheme: scheme, secret: &credentialSecret{value: value}}
}

// Value returns the secret only for explicit use at the provider boundary.
func (c Credential) Value() string {
	if c.secret == nil {
		return ""
	}
	return c.secret.value
}

func (c Credential) hasValue() bool {
	return c.secret != nil && c.secret.value != ""
}

func (c Credential) String() string {
	return fmt.Sprintf("people provider credential (%s)", c.Scheme)
}

func (c Credential) GoString() string {
	return c.String()
}

// Format prevents supported fmt verbs from inspecting credential internals.
func (c Credential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// CredentialStore owns private credential lifecycle independently of config.
type CredentialStore interface {
	Save(profileName string, credential Credential) error
	Load(profileName string) (Credential, error)
	PreflightDelete(profileName string) error
	Delete(profileName string) error
}

// CredentialResolver resolves only the source fingerprinted into a profile.
type CredentialResolver interface {
	Resolve(profileName string, profile ProviderProfile) (Credential, error)
}

// FileCredentialStore stores provider credentials beneath a private namespace.
type FileCredentialStore struct {
	tokensDir string
	hooks     *credentialStoreHooks
}

type credentialStoreHooks struct {
	afterNamespaceParentSync func()
	afterLockAcquired        func()
	beforeOperation          func(string)
	afterCandidateOpen       func(string)
	beforeCandidatePublish   func(string)
	failedCandidateTruncate  func() error
	failedCandidateSync      func() error
	afterCredentialOpen      func(string)
	beforeCredentialRetire   func()
}

type credentialStoreRoot interface {
	save(profileName string, data []byte) error
	load(profileName string) ([]byte, error)
	preflightDelete(profileName string) error
	delete(profileName string) error
}

type credentialFile struct {
	Scheme AuthScheme `json:"scheme"`
	Value  string     `json:"value"`
}

// NewFileCredentialStore constructs a store rooted at
// <tokensDir>/people-providers.
func NewFileCredentialStore(tokensDir string) *FileCredentialStore {
	return &FileCredentialStore{tokensDir: tokensDir}
}

// Save validates and atomically publishes one exact named credential.
func (s *FileCredentialStore) Save(profileName string, credential Credential) error {
	if err := validateCredentialProfileName(profileName); err != nil {
		return err
	}
	if err := validateStoredCredential(credential); err != nil {
		return err
	}
	data, err := json.Marshal(credentialFile{Scheme: credential.Scheme, Value: credential.Value()}) //nolint:gosec // serialized only into the private 0600 store
	if err != nil {
		return errors.New("serialize people provider credential")
	}
	return s.withCredentialRoot("save", func(root credentialStoreRoot) error {
		return root.save(profileName, data)
	})
}

// SaveNew atomically publishes a credential only when the exact named record
// is absent. The existence check and publication share the credential-root
// lock so concurrent setup processes cannot overwrite the winner.
func (s *FileCredentialStore) SaveNew(profileName string, credential Credential) (bool, error) {
	if err := validateCredentialProfileName(profileName); err != nil {
		return false, err
	}
	if err := validateStoredCredential(credential); err != nil {
		return false, err
	}
	data, err := json.Marshal(credentialFile{Scheme: credential.Scheme, Value: credential.Value()}) //nolint:gosec // serialized only into the private 0600 store
	if err != nil {
		return false, errors.New("serialize people provider credential")
	}
	created := false
	err = s.withCredentialRoot("save-new", func(root credentialStoreRoot) error {
		if _, loadErr := root.load(profileName); loadErr == nil {
			return nil
		} else if !errors.Is(loadErr, ErrCredentialNotFound) {
			return loadErr
		}
		if saveErr := root.save(profileName, data); saveErr != nil {
			return saveErr
		}
		created = true
		return nil
	})
	return created, err
}

// Load reads and validates one exact named credential.
func (s *FileCredentialStore) Load(profileName string) (Credential, error) {
	if err := validateCredentialProfileName(profileName); err != nil {
		return Credential{}, err
	}
	var credential Credential
	err := s.withCredentialRoot("load", func(root credentialStoreRoot) error {
		data, err := root.load(profileName)
		if err != nil {
			return err
		}
		var stored credentialFile
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&stored); err != nil {
			return fmt.Errorf("parse people provider credential for profile %q: malformed JSON", profileName)
		}
		if err := requireCredentialJSONEnd(decoder); err != nil {
			return fmt.Errorf("parse people provider credential for profile %q: malformed JSON", profileName)
		}
		credential = NewCredential(stored.Scheme, stored.Value)
		if err := validateStoredCredential(credential); err != nil {
			return fmt.Errorf("validate people provider credential for profile %q: %w", profileName, err)
		}
		return nil
	})
	return credential, err
}

// PreflightDelete validates the current credential namespace and exact target
// for deletion without reading or modifying credential contents. Delete
// repeats all checks because filesystem state can change after this returns.
func (s *FileCredentialStore) PreflightDelete(profileName string) error {
	if err := validateCredentialProfileName(profileName); err != nil {
		return err
	}
	return s.withCredentialRoot("preflight-delete", func(root credentialStoreRoot) error {
		return root.preflightDelete(profileName)
	})
}

// Delete removes only the exact named record and is idempotent when absent.
func (s *FileCredentialStore) Delete(profileName string) error {
	if err := validateCredentialProfileName(profileName); err != nil {
		return err
	}
	return s.withCredentialRoot("delete", func(root credentialStoreRoot) error {
		return root.delete(profileName)
	})
}

// ValidateProviderProfileName applies the single grammar used by provider
// config, commands, and the private credential namespace.
func ValidateProviderProfileName(profileName string) error {
	if !credentialProfileNamePattern.MatchString(profileName) {
		return errors.New("invalid people provider profile name")
	}
	return nil
}

func validateCredentialProfileName(profileName string) error {
	if err := ValidateProviderProfileName(profileName); err != nil {
		return fmt.Errorf("invalid people provider credential profile name: %w", err)
	}
	return nil
}

func validateStoredCredential(credential Credential) error {
	if credential.Value() == "" {
		return errors.New("people provider credential value is empty")
	}
	switch credential.Scheme {
	case AuthBearer, AuthXAPIKey, AuthGoogleAPIKey:
		return nil
	default:
		return errors.New("people provider credential has an invalid authentication scheme")
	}
}

func requireCredentialJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("credential file contains multiple JSON values")
		}
		return err
	}
	return nil
}

type credentialResolver struct {
	store  CredentialStore
	lookup CredentialLookup
}

// NewCredentialResolver constructs the sole credential-source dispatcher.
func NewCredentialResolver(store CredentialStore, lookup CredentialLookup) CredentialResolver {
	return &credentialResolver{store: store, lookup: lookup}
}

func (r *credentialResolver) Resolve(profileName string, profile ProviderProfile) (Credential, error) {
	if err := profile.Validate(); err != nil {
		return Credential{}, fmt.Errorf("validate people provider profile before credential resolution: %w", err)
	}
	switch profile.Credential {
	case CredentialStored:
		if profileName != profile.CredentialRef {
			return Credential{}, errors.New("stored people provider credential name does not match the fingerprinted profile")
		}
		if r.store == nil {
			return Credential{}, errors.New("people provider credential store is unavailable")
		}
		credential, err := r.store.Load(profileName)
		if err != nil {
			return Credential{}, err
		}
		if credential.Scheme != profile.Auth {
			return Credential{}, errors.New("stored people provider credential scheme does not match the fingerprinted profile")
		}
		return credential, nil
	case CredentialEnv:
		if r.lookup == nil {
			return Credential{}, errors.New("people provider credential environment lookup is unavailable")
		}
		value, ok := r.lookup(profile.CredentialRef)
		if !ok || value == "" {
			return Credential{}, fmt.Errorf("people provider credential environment variable %s is not set", profile.CredentialRef)
		}
		return NewCredential(profile.Auth, value), nil
	case CredentialNone:
		return NewCredential(AuthNone, ""), nil
	default:
		return Credential{}, errors.New("people provider profile has an invalid credential source")
	}
}
