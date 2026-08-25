package peoplesweep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
)

var (
	credentialProfileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	ErrCredentialNotFound        = errors.New("people provider credential not found")
)

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
	Delete(profileName string) error
}

// CredentialResolver resolves only the source fingerprinted into a profile.
type CredentialResolver interface {
	Resolve(profileName string, profile ProviderProfile) (Credential, error)
}

// FileCredentialStore stores provider credentials beneath a private namespace.
type FileCredentialStore struct {
	tokensDir string
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
	return s.withLock(true, func(root string) error {
		path := filepath.Join(root, profileName+".json")
		if err := validateCredentialFilePath(path, true); err != nil {
			return err
		}
		data, err := json.Marshal(credentialFile{Scheme: credential.Scheme, Value: credential.Value()}) //nolint:gosec // serialized only into the private 0600 store
		if err != nil {
			return errors.New("serialize people provider credential")
		}
		candidate, err := os.CreateTemp(root, ".credential-*.tmp")
		if err != nil {
			return fmt.Errorf("create people provider credential candidate: %w", err)
		}
		candidatePath := candidate.Name()
		defer func() { _ = os.Remove(candidatePath) }()
		if err := fileutil.SecureChmod(candidatePath, 0o600); err != nil {
			_ = candidate.Close()
			return fmt.Errorf("protect people provider credential candidate: %w", err)
		}
		if _, err := candidate.Write(data); err != nil {
			_ = candidate.Close()
			return fmt.Errorf("write people provider credential candidate: %w", err)
		}
		if err := candidate.Sync(); err != nil {
			_ = candidate.Close()
			return fmt.Errorf("sync people provider credential candidate: %w", err)
		}
		if err := candidate.Close(); err != nil {
			return fmt.Errorf("close people provider credential candidate: %w", err)
		}
		if err := validateCredentialFilePath(path, true); err != nil {
			return err
		}
		if err := os.Rename(candidatePath, path); err != nil {
			return fmt.Errorf("publish people provider credential: %w", err)
		}
		if err := syncCredentialDirectory(root); err != nil {
			return err
		}
		return nil
	})
}

// Load reads and validates one exact named credential.
func (s *FileCredentialStore) Load(profileName string) (Credential, error) {
	if err := validateCredentialProfileName(profileName); err != nil {
		return Credential{}, err
	}
	var credential Credential
	err := s.withLock(true, func(root string) error {
		path := filepath.Join(root, profileName+".json")
		if err := validateCredentialFilePath(path, false); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
			}
			return fmt.Errorf("read people provider credential for profile %q: %w", profileName, err)
		}
		var stored credentialFile
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&stored); err != nil {
			return fmt.Errorf("parse people provider credential for profile %q: %w", profileName, err)
		}
		if err := requireCredentialJSONEnd(decoder); err != nil {
			return fmt.Errorf("parse people provider credential for profile %q: %w", profileName, err)
		}
		credential = NewCredential(stored.Scheme, stored.Value)
		if err := validateStoredCredential(credential); err != nil {
			return fmt.Errorf("validate people provider credential for profile %q: %w", profileName, err)
		}
		return nil
	})
	return credential, err
}

// Delete removes only the exact named record and is idempotent when absent.
func (s *FileCredentialStore) Delete(profileName string) error {
	if err := validateCredentialProfileName(profileName); err != nil {
		return err
	}
	return s.withLock(true, func(root string) error {
		path := filepath.Join(root, profileName+".json")
		if err := validateCredentialFilePath(path, true); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove people provider credential for profile %q: %w", profileName, err)
		}
		return syncCredentialDirectory(root)
	})
}

func (s *FileCredentialStore) withLock(create bool, operation func(string) error) (retErr error) {
	root, before, err := s.inspectRoot(create)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(root, ".credentials.lock")
	if err := validateCredentialLockPath(lockPath); err != nil {
		return err
	}
	storeLock := flock.New(lockPath, flock.SetPermissions(0o600))
	if err := storeLock.Lock(); err != nil {
		return fmt.Errorf("lock people provider credential store: %w", err)
	}
	defer func() {
		if err := storeLock.Unlock(); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock people provider credential store: %w", err)
		}
	}()
	if err := fileutil.SecureChmod(lockPath, 0o600); err != nil {
		return fmt.Errorf("protect people provider credential lock: %w", err)
	}
	if err := validateCredentialLockPath(lockPath); err != nil {
		return err
	}
	_, after, err := s.inspectRoot(false)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return errors.New("people provider credential directory changed while locking")
	}
	return operation(root)
}

func (s *FileCredentialStore) inspectRoot(create bool) (string, os.FileInfo, error) {
	if s == nil || filepath.Clean(s.tokensDir) == "." || s.tokensDir == "" {
		return "", nil, errors.New("people provider credential tokens directory is required")
	}
	if err := inspectDirectDirectory(s.tokensDir, "tokens", create); err != nil {
		return "", nil, err
	}
	root := filepath.Join(s.tokensDir, "people-providers")
	if err := inspectDirectDirectory(root, "credential", create); err != nil {
		return "", nil, err
	}
	if err := fileutil.SecureChmod(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("protect people provider credential directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", nil, fmt.Errorf("inspect people provider credential directory: %w", err)
	}
	return root, info, nil
}

func inspectDirectDirectory(path, kind string, create bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) || !create {
			return fmt.Errorf("inspect people provider %s directory: %w", kind, err)
		}
		if err := fileutil.SecureMkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create people provider %s directory: %w", kind, err)
		}
		info, err = os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect people provider %s directory: %w", kind, err)
		}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("people provider %s path is not a direct directory", kind)
	}
	return nil
}

func validateCredentialProfileName(profileName string) error {
	if !credentialProfileNamePattern.MatchString(profileName) {
		return fmt.Errorf("invalid people provider credential profile name %q", profileName)
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

func validateCredentialFilePath(path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if allowMissing && os.IsNotExist(err) {
			return nil
		}
		if os.IsNotExist(err) {
			return fmt.Errorf("%w for profile %q", ErrCredentialNotFound, credentialProfileNameFromPath(path))
		}
		return fmt.Errorf("inspect people provider credential file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("people provider credential path is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("people provider credential file permissions are not private")
	}
	return nil
}

func credentialProfileNameFromPath(path string) string {
	return filepath.Base(path[:len(path)-len(filepath.Ext(path))])
}

func validateCredentialLockPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect people provider credential lock: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("people provider credential lock is not a regular file")
	}
	return nil
}

func syncCredentialDirectory(root string) error {
	directory, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("open people provider credential directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync people provider credential directory: %w", err)
	}
	return nil
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
