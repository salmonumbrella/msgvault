//go:build linux || darwin

package peoplesweep

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const credentialSecurityTestValue = "credential-security-test-value"

func TestCredentialStoreSaveUsesPinnedNamespaceAfterPathSwap(t *testing.T) {
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	retained := filepath.Join(tokensDir, "retained-people-providers")
	external := t.TempDir()
	var once sync.Once
	store.hooks = &credentialStoreHooks{beforeOperation: func(operation string) {
		if operation != "save" {
			return
		}
		once.Do(func() {
			require.NoError(t, os.Rename(root, retained))
			require.NoError(t, os.Symlink(external, root))
		})
	}}

	err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(external, "profile.json"))
	contents, err := os.ReadFile(filepath.Join(retained, "profile.json"))
	require.NoError(t, err)
	assert.True(t, len(contents) > 0, "credential was not published in the pinned namespace")
}

func TestCredentialStoreLoadUsesPinnedEntryAfterSwap(t *testing.T) {
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	original := filepath.Join(root, "profile.original")
	external := filepath.Join(t.TempDir(), "replacement.json")
	require.NoError(t, os.WriteFile(external, []byte(`{"scheme":"bearer","value":"replacement"}`), 0o600))
	var once sync.Once
	store.hooks = &credentialStoreHooks{afterCredentialOpen: func(operation string) {
		if operation != "load" {
			return
		}
		once.Do(func() {
			require.NoError(t, os.Rename(filepath.Join(root, "profile.json"), original))
			require.NoError(t, os.Symlink(external, filepath.Join(root, "profile.json")))
		})
	}}

	credential, err := store.Load("profile")
	require.NoError(t, err)
	assert.True(t, credential.Value() == credentialSecurityTestValue,
		"load did not read the pinned credential entry")
}

func TestCredentialStoreSaveRefusesSwappedCandidate(t *testing.T) {
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	external := filepath.Join(t.TempDir(), "must-remain")
	require.NoError(t, os.WriteFile(external, []byte("unchanged"), 0o600))
	var retained string
	store.hooks = &credentialStoreHooks{afterCandidateOpen: func(candidateName string) {
		retained = filepath.Join(root, candidateName+".retained")
		require.NoError(t, os.Rename(filepath.Join(root, candidateName), retained))
		require.NoError(t, os.Symlink(external, filepath.Join(root, candidateName)))
	}}

	err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.ErrorContains(t, err, "candidate changed")
	contents, readErr := os.ReadFile(external)
	require.NoError(t, readErr)
	assert.Equal(t, "unchanged", string(contents))
	assert.FileExists(t, retained)
	assert.NoFileExists(t, filepath.Join(root, "profile.json"))
}

func TestCredentialStoreSaveDetectsCandidateSwapAtPublicationBoundary(t *testing.T) {
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	external := filepath.Join(t.TempDir(), "must-remain")
	require.NoError(t, os.WriteFile(external, []byte("unchanged"), 0o600))
	var retained string
	store.hooks = &credentialStoreHooks{beforeCandidatePublish: func(candidateName string) {
		retained = filepath.Join(root, candidateName+".retained")
		require.NoError(t, os.Rename(filepath.Join(root, candidateName), retained))
		require.NoError(t, os.Symlink(external, filepath.Join(root, candidateName)))
	}}

	err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.ErrorContains(t, err, "candidate changed")
	contents, readErr := os.ReadFile(external)
	require.NoError(t, readErr)
	assert.Equal(t, "unchanged", string(contents))
	retainedInfo, statErr := os.Stat(retained)
	require.NoError(t, statErr)
	assert.Zero(t, retainedInfo.Size(), "the pinned candidate was not wiped after the publication race")
	_, loadErr := store.Load("profile")
	require.Error(t, loadErr)
}

func TestCredentialStoreDeleteRefusesSwappedEntry(t *testing.T) {
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	original := filepath.Join(root, "profile.original")
	external := filepath.Join(t.TempDir(), "must-remain")
	require.NoError(t, os.WriteFile(external, []byte("unchanged"), 0o600))
	var once sync.Once
	store.hooks = &credentialStoreHooks{afterCredentialOpen: func(operation string) {
		if operation != "delete" {
			return
		}
		once.Do(func() {
			require.NoError(t, os.Rename(filepath.Join(root, "profile.json"), original))
			require.NoError(t, os.Symlink(external, filepath.Join(root, "profile.json")))
		})
	}}

	err := store.Delete("profile")
	require.ErrorContains(t, err, "changed")
	contents, readErr := os.ReadFile(external)
	require.NoError(t, readErr)
	assert.Equal(t, "unchanged", string(contents))
	assert.FileExists(t, original)
}

func TestCredentialStoreDeleteDetectsSwapAtRemovalBoundary(t *testing.T) {
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	original := filepath.Join(root, "profile.original")
	external := filepath.Join(t.TempDir(), "must-remain")
	require.NoError(t, os.WriteFile(external, []byte("unchanged"), 0o600))
	store.hooks = &credentialStoreHooks{beforeCredentialRetire: func() {
		require.NoError(t, os.Rename(filepath.Join(root, "profile.json"), original))
		require.NoError(t, os.Symlink(external, filepath.Join(root, "profile.json")))
	}}

	err := store.Delete("profile")
	require.ErrorContains(t, err, "changed")
	contents, readErr := os.ReadFile(external)
	require.NoError(t, readErr)
	assert.Equal(t, "unchanged", string(contents))
	originalInfo, statErr := os.Stat(original)
	require.NoError(t, statErr)
	assert.Zero(t, originalInfo.Size(), "the pinned credential was not wiped after the deletion race")
}

func TestCredentialStoreRejectsFIFOWithoutBlocking(t *testing.T) {
	for _, operation := range []string{"save", "load", "delete"} {
		t.Run(operation, func(t *testing.T) {
			tokensDir := t.TempDir()
			store := NewFileCredentialStore(tokensDir)
			require.NoError(t, store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
			fifo := filepath.Join(tokensDir, "people-providers", "profile.json")
			require.NoError(t, unix.Mkfifo(fifo, 0o600))

			result := make(chan error, 1)
			go func() {
				switch operation {
				case "save":
					result <- store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
				case "load":
					_, err := store.Load("profile")
					result <- err
				case "delete":
					result <- store.Delete("profile")
				}
			}()

			select {
			case err := <-result:
				require.ErrorContains(t, err, "not a regular file")
			case <-time.After(time.Second):
				assert.Fail(t, "credential operation blocked while inspecting a FIFO")
			}
		})
	}
}

func TestCredentialStoreLockReplacementCannotSplitExclusion(t *testing.T) {
	tokensDir := t.TempDir()
	first := NewFileCredentialStore(tokensDir)
	require.NoError(t, first.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	firstLocked := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	hookResult := make(chan error, 1)
	var replaceOnce sync.Once
	first.hooks = &credentialStoreHooks{afterLockAcquired: func() {
		replaceOnce.Do(func() {
			hookErr := os.Rename(filepath.Join(root, ".credentials.lock"),
				filepath.Join(root, ".credentials.lock.original"))
			if hookErr == nil {
				hookErr = os.WriteFile(filepath.Join(root, ".credentials.lock"), nil, 0o600)
			}
			hookResult <- hookErr
			close(firstLocked)
			<-releaseFirst
		})
	}}
	second := NewFileCredentialStore(tokensDir)
	second.hooks = &credentialStoreHooks{beforeOperation: func(string) {
		select {
		case <-secondEntered:
		default:
			close(secondEntered)
		}
	}}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- first.Save("first", NewCredential(AuthBearer, credentialSecurityTestValue))
	}()
	<-firstLocked
	require.NoError(t, <-hookResult)
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- second.Save("second", NewCredential(AuthBearer, credentialSecurityTestValue))
	}()
	select {
	case <-secondEntered:
		assert.Fail(t, "replacement lock admitted a concurrent credential operation")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	require.NoError(t, <-firstResult)
	require.NoError(t, <-secondResult)
}

func TestCredentialStoreSyncsNamespaceParentAfterCreation(t *testing.T) {
	store := NewFileCredentialStore(t.TempDir())
	var parentSynced atomic.Bool
	store.hooks = &credentialStoreHooks{afterNamespaceParentSync: func() {
		parentSynced.Store(true)
	}}
	require.NoError(t, store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	assert.True(t, parentSynced.Load(), "credential namespace parent was not synced after creation")
}
