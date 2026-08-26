//go:build linux || darwin

package peoplesweep_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

const credentialCanary = "test-credential-canary"

func TestValidateProviderProfileNameUsesOneSafeGrammar(t *testing.T) {
	for _, name := range []string{"a", "Alpha_1", "profile.with-dots", strings.Repeat("z", 64)} {
		assert.NoError(t, peoplesweep.ValidateProviderProfileName(name), name)
	}
	for _, name := range []string{"", "--help", "--json", " leading", "trailing ", "bad\nname", strings.Repeat("z", 65)} {
		err := peoplesweep.ValidateProviderProfileName(name)
		assert.Error(t, err, name)
		if name != "" {
			assert.NotContains(t, err.Error(), name, "unsafe input must not be reflected")
		}
	}
}

func TestCredentialNeverFormatsSecret(t *testing.T) {
	credential := peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)
	formatted := fmt.Sprintf("%v %#v %s %q %x %p", credential, credential, credential, credential, credential, credential)
	assert.NotContains(t, formatted, credentialCanary)

	var captured bytes.Buffer
	logger := log.New(&captured, "", 0)
	logger.Printf("credential=%v", credential)
	assert.NotContains(t, captured.String(), credentialCanary)

	profile := credentialTestProfile(t, peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer)
	profileJSON, err := json.Marshal(profile)
	require.NoError(t, err)
	assert.NotContains(t, string(profileJSON), credentialCanary)

	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	err = store.Save("../invalid", credential)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), credentialCanary)
}

func TestCredentialStoreLifecycleUsesPrivateFilesAndExactDeletion(t *testing.T) {
	tokensDir := t.TempDir()
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("alpha", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)))
	require.NoError(t, store.Save("alpha.backup", peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, credentialCanary)))

	root := filepath.Join(tokensDir, "people-providers")
	rootInfo, err := os.Stat(root)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), rootInfo.Mode().Perm())
	for _, name := range []string{".credentials.lock", "alpha.json", "alpha.backup.json"} {
		info, statErr := os.Stat(filepath.Join(root, name))
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}

	loaded, err := store.Load("alpha")
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthBearer, loaded.Scheme)
	assert.True(t, loaded.Value() == credentialCanary, "loaded credential differs")

	require.NoError(t, store.Delete("alpha"))
	_, err = store.Load("alpha")
	require.Error(t, err)
	remaining, err := store.Load("alpha.backup")
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthXAPIKey, remaining.Scheme)
	assert.True(t, remaining.Value() == credentialCanary, "remaining credential differs")
	require.NoError(t, store.Delete("alpha"))
}

func TestCredentialStoreRotatesAtomically(t *testing.T) {
	tokensDir := t.TempDir()
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-old")))

	var wg sync.WaitGroup
	errors := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			credential, err := store.Load("rotate")
			if err != nil {
				select {
				case errors <- err:
				default:
				}
				return
			}
			value := credential.Value()
			if value != credentialCanary+"-old" && value != credentialCanary+"-new" {
				select {
				case errors <- fmt.Errorf("reader observed a partial credential"):
				default:
				}
				return
			}
		}
	}()
	for range 100 {
		require.NoError(t, store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-new")))
		require.NoError(t, store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-old")))
	}
	wg.Wait()
	select {
	case err := <-errors:
		require.NoError(t, err)
	default:
	}
}

func TestCredentialStoreSerializesConcurrentSaves(t *testing.T) {
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	const profiles = 16
	var wg sync.WaitGroup
	errors := make(chan error, profiles)
	for index := range profiles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("profile-%02d", index)
			errors <- store.Save(name, peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary))
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	for index := range profiles {
		credential, err := store.Load(fmt.Sprintf("profile-%02d", index))
		require.NoError(t, err)
		assert.Equal(t, peoplesweep.AuthBearer, credential.Scheme)
		assert.True(t, credential.Value() == credentialCanary, "loaded credential differs")
	}
}

func TestCredentialStoreSaveNewNeverOverwritesConcurrentWinner(t *testing.T) {
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	start := make(chan struct{})
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	for _, value := range []string{credentialCanary + "-first", credentialCanary + "-second"} {
		go func() {
			<-start
			created, err := store.SaveNew("new-profile", peoplesweep.NewCredential(
				peoplesweep.AuthBearer, value,
			))
			results <- result{created: created, err: err}
		}()
	}
	close(start)
	created := 0
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		if result.created {
			created++
		}
	}
	assert.Equal(t, 1, created)
	credential, err := store.Load("new-profile")
	require.NoError(t, err)
	assert.Contains(t, []string{credentialCanary + "-first", credentialCanary + "-second"}, credential.Value())
}

func TestCredentialStoreRejectsInvalidNamesAndMalformedJSON(t *testing.T) {
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	for _, name := range []string{"", ".hidden", "../escape", "slash/name", "space name", string(make([]byte, 65))} {
		err := store.Save(name, peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary))
		require.Error(t, err, name)
		assert.NotContains(t, err.Error(), credentialCanary)
	}

	root := filepath.Join(t.TempDir(), "people-providers")
	store = peoplesweep.NewFileCredentialStore(filepath.Dir(root))
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "broken.json"), []byte(`{"scheme":"bearer","value":`), 0o600))
	_, err := store.Load("broken")
	require.ErrorContains(t, err, "parse")
	assert.NotContains(t, err.Error(), credentialCanary)

	require.NoError(t, os.WriteFile(filepath.Join(root, "unknown.json"), []byte(fmt.Sprintf(
		`{"scheme":"bearer","value":"safe-test-value",%q:true}`, credentialCanary)), 0o600))
	_, err = store.Load("unknown")
	require.Error(t, err)
	if strings.Contains(err.Error(), credentialCanary) {
		assert.Fail(t, "credential parse error disclosed an attacker-controlled field name")
	}
}

func TestCredentialStoreRejectsSymlinkedRootFileAndLock(t *testing.T) {
	credential := peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)

	t.Run("tokens directory", func(t *testing.T) {
		parent := t.TempDir()
		target := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		require.NoError(t, os.Symlink(target, tokensDir))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(t, err, "directory")
		assert.NotContains(t, err.Error(), credentialCanary)
	})

	t.Run("root", func(t *testing.T) {
		tokensDir := t.TempDir()
		target := t.TempDir()
		require.NoError(t, os.Symlink(target, filepath.Join(tokensDir, "people-providers")))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(t, err, "directory")
		assert.NotContains(t, err.Error(), credentialCanary)
	})

	t.Run("credential file", func(t *testing.T) {
		tokensDir := t.TempDir()
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(t, os.Mkdir(root, 0o700))
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(root, "profile.json")))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(t, err, "regular file")
		contents, readErr := os.ReadFile(target)
		require.NoError(t, readErr)
		assert.Equal(t, "unchanged", string(contents))
		assert.NotContains(t, err.Error(), credentialCanary)
	})

	t.Run("lock", func(t *testing.T) {
		tokensDir := t.TempDir()
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(t, os.Mkdir(root, 0o700))
		target := filepath.Join(t.TempDir(), "lock-target")
		require.NoError(t, os.WriteFile(target, nil, 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(root, ".credentials.lock")))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(t, err, "lock")
		assert.NotContains(t, err.Error(), credentialCanary)
	})
}

func TestCredentialResolverUsesStoredEnvironmentAndNoneSources(t *testing.T) {
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	require.NoError(t, store.Save("stored-profile", peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, credentialCanary)))
	lookup := func(name string) (string, bool) {
		if name != "TEST_CREDENTIAL" {
			return "", false
		}
		return credentialCanary, true
	}
	resolver := peoplesweep.NewCredentialResolver(store, lookup)

	stored, err := resolver.Resolve("stored-profile", credentialTestProfile(t,
		peoplesweep.CredentialStored, "stored-profile", peoplesweep.AuthXAPIKey))
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthXAPIKey, stored.Scheme)
	assert.True(t, stored.Value() == credentialCanary, "stored credential differs")

	environment, err := resolver.Resolve("ignored-profile-name", credentialTestProfile(t,
		peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer))
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthBearer, environment.Scheme)
	assert.True(t, environment.Value() == credentialCanary, "environment credential differs")

	none, err := resolver.Resolve("local", credentialTestProfile(t,
		peoplesweep.CredentialNone, "", peoplesweep.AuthNone))
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthNone, none.Scheme)
	assert.Empty(t, none.Value())
}

func TestCredentialResolverFailsClosedWithoutLeakingSecrets(t *testing.T) {
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	require.NoError(t, store.Save("mismatch", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)))
	resolver := peoplesweep.NewCredentialResolver(store, func(string) (string, bool) {
		return credentialCanary, false
	})

	_, err := resolver.Resolve("mismatch", credentialTestProfile(t,
		peoplesweep.CredentialStored, "mismatch", peoplesweep.AuthXAPIKey))
	require.ErrorContains(t, err, "scheme")
	assert.NotContains(t, err.Error(), credentialCanary)

	_, err = resolver.Resolve("environment", credentialTestProfile(t,
		peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer))
	require.ErrorContains(t, err, "TEST_CREDENTIAL")
	assert.NotContains(t, err.Error(), credentialCanary)

	remote := credentialTestProfile(t, peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer)
	remote.Credential = peoplesweep.CredentialNone
	remote.CredentialRef = ""
	remote.Auth = peoplesweep.AuthNone
	_, err = resolver.Resolve("remote", remote)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), credentialCanary)
}

func credentialTestProfile(
	t *testing.T,
	source peoplesweep.CredentialSource,
	reference string,
	auth peoplesweep.AuthScheme,
) peoplesweep.ProviderProfile {
	t.Helper()
	config := validConfig()
	mutateActiveProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.Endpoint = "https://provider.example.test/v1"
		provider.Auth = auth
		provider.Credential = source
		provider.CredentialEnv = ""
		if source == peoplesweep.CredentialEnv {
			provider.CredentialEnv = reference
		}
		if source == peoplesweep.CredentialNone {
			provider.Endpoint = "http://127.0.0.1:11434/v1"
		}
	})
	if source == peoplesweep.CredentialStored {
		oldName := config.Provider.Name
		config.Provider.Name = reference
		provider := config.Providers[oldName]
		delete(config.Providers, oldName)
		config.Providers[reference] = provider
	}
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}
