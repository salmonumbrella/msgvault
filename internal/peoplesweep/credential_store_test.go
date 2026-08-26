//go:build linux || darwin

package peoplesweep_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"golang.org/x/sys/unix"
)

const credentialCanary = "test-credential-canary"

type credentialPathSnapshot struct {
	exists        bool
	info          os.FileInfo
	entries       []string
	contentDigest [sha256.Size]byte
}

func snapshotCredentialPaths(t *testing.T, paths ...string) map[string]credentialPathSnapshot {
	t.Helper()
	snapshot := make(map[string]credentialPathSnapshot, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshot[path] = credentialPathSnapshot{}
			continue
		}
		require.NoError(t, err)
		state := credentialPathSnapshot{exists: true, info: info}
		if info.IsDir() {
			entries, readErr := os.ReadDir(path)
			require.NoError(t, readErr)
			state.entries = make([]string, 0, len(entries))
			for _, entry := range entries {
				state.entries = append(state.entries, entry.Name())
			}
		} else if info.Mode().IsRegular() {
			contents, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			state.contentDigest = sha256.Sum256(contents)
		}
		snapshot[path] = state
	}
	return snapshot
}

func assertCredentialPathsUnchanged(
	t *testing.T,
	before map[string]credentialPathSnapshot,
) {
	t.Helper()
	for path, want := range before {
		gotInfo, err := os.Lstat(path)
		if !want.exists {
			require.ErrorIs(t, err, os.ErrNotExist, path)
			continue
		}
		require.NoError(t, err, path)
		assert.True(t, os.SameFile(want.info, gotInfo), "%s inode changed", path)
		assert.Equal(t, want.info.Mode(), gotInfo.Mode(), "%s mode changed", path)
		assert.Equal(t, want.info.Size(), gotInfo.Size(), "%s size changed", path)
		assert.Equal(t, want.info.ModTime(), gotInfo.ModTime(), "%s mtime changed", path)
		if want.info.IsDir() {
			entries, readErr := os.ReadDir(path)
			require.NoError(t, readErr, path)
			gotEntries := make([]string, 0, len(entries))
			for _, entry := range entries {
				gotEntries = append(gotEntries, entry.Name())
			}
			assert.Equal(t, want.entries, gotEntries, "%s entries changed", path)
		} else if want.info.Mode().IsRegular() {
			contents, readErr := os.ReadFile(path)
			require.NoError(t, readErr, path)
			assert.Equal(t, want.contentDigest, sha256.Sum256(contents), "%s contents changed", path)
		}
	}
}

func validateCredentialDeletePreflight(store peoplesweep.CredentialStore) error {
	guard, err := store.PreflightDelete("profile")
	if err != nil {
		return err
	}
	return guard.Close()
}

func guardedCredentialDelete(
	store peoplesweep.CredentialStore,
	profileName string,
) error {
	guard, err := store.PreflightDelete(profileName)
	if err != nil {
		return err
	}
	return errors.Join(store.Delete(profileName, guard), guard.Close())
}

func createExistingCredentialPreflightFixture(t *testing.T, tokensDir string) []string {
	t.Helper()
	root := filepath.Join(tokensDir, "people-providers")
	lockPath := filepath.Join(root, ".credentials.lock")
	credentialPath := filepath.Join(root, "profile.json")
	require.NoError(t, os.Chmod(tokensDir, 0o700))
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.WriteFile(credentialPath, []byte(credentialCanary), 0o600))
	return []string{tokensDir, root, lockPath, credentialPath}
}

func TestValidateProviderProfileNameUsesOneSafeGrammar(t *testing.T) {
	for _, name := range []string{"a", "Alpha_1", "profile.with-dots", strings.Repeat("z", 64)} {
		require.NoError(t, peoplesweep.ValidateProviderProfileName(name), name)
	}
	for _, name := range []string{"", "--help", "--json", " leading", "trailing ", "bad\nname", strings.Repeat("z", 65)} {
		err := peoplesweep.ValidateProviderProfileName(name)
		require.Error(t, err, name)
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
	assert.Equal(t, credentialCanary, loaded.Value(), "loaded credential differs")

	require.NoError(t, guardedCredentialDelete(store, "alpha"))
	_, err = store.Load("alpha")
	require.Error(t, err)
	remaining, err := store.Load("alpha.backup")
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthXAPIKey, remaining.Scheme)
	assert.Equal(t, credentialCanary, remaining.Value(), "remaining credential differs")
	require.ErrorIs(t, guardedCredentialDelete(store, "alpha"), peoplesweep.ErrCredentialNotFound)
}

func TestCredentialStoreDeleteRetiresOnlyExactPinnedTargetAsBoundedTombstone(t *testing.T) {
	tokensDir := t.TempDir()
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("profile", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, credentialCanary,
	)))
	require.NoError(t, store.Save("other", peoplesweep.NewCredential(
		peoplesweep.AuthXAPIKey, credentialCanary+"-other",
	)))
	guard, err := store.PreflightDelete("profile")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, guard.Close()) })

	root := filepath.Join(tokensDir, "people-providers")
	lockPath := filepath.Join(root, ".credentials.lock")
	targetPath := filepath.Join(root, "profile.json")
	otherPath := filepath.Join(root, "other.json")
	targetBefore, err := os.Stat(targetPath)
	require.NoError(t, err)
	stableBefore := snapshotCredentialPaths(t, tokensDir, root, lockPath, otherPath)

	require.NoError(t, store.Delete("profile", guard))

	targetAfter, err := os.Lstat(targetPath)
	require.NoError(t, err)
	assert.True(t, os.SameFile(targetBefore, targetAfter), "deletion replaced the exact target inode")
	assert.True(t, targetAfter.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o600), targetAfter.Mode().Perm())
	assert.Zero(t, targetAfter.Size())
	var targetStat unix.Stat_t
	require.NoError(t, unix.Lstat(targetPath, &targetStat))
	assert.Equal(t, uint64(1), targetStat.Nlink)
	tombstone, err := os.ReadFile(targetPath)
	require.NoError(t, err)
	assert.Empty(t, tombstone, "credential tombstone retained bytes")
	assertCredentialPathsUnchanged(t, stableBefore)
	require.NoError(t, guard.Close())
	require.ErrorIs(t, validateCredentialDeletePreflight(store), peoplesweep.ErrCredentialNotFound)
}

func TestCredentialStorePreflightDeleteValidatesWithoutReadingOrChangingSecret(t *testing.T) {
	tokensDir := t.TempDir()
	paths := createExistingCredentialPreflightFixture(t, tokensDir)
	credentialPath := filepath.Join(tokensDir, "people-providers", "profile.json")
	beforeContents, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	before := snapshotCredentialPaths(t, paths...)

	err = validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
	require.NoError(t, err)
	afterContents, err := os.ReadFile(credentialPath)
	require.NoError(t, err)
	assert.Equal(t, beforeContents, afterContents)
	assertCredentialPathsUnchanged(t, before)
}

func TestCredentialStorePreflightDeleteRejectsMissingStateWithoutCreatingIt(t *testing.T) {
	t.Run("tokens root", func(t *testing.T) {
		parent := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		before := snapshotCredentialPaths(t, parent, tokensDir)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential namespace", func(t *testing.T) {
		tokensDir := t.TempDir()
		require.NoError(t, os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		before := snapshotCredentialPaths(t, tokensDir, root)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("lock marker", func(t *testing.T) {
		tokensDir := t.TempDir()
		require.NoError(t, os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		credentialPath := filepath.Join(root, "profile.json")
		require.NoError(t, os.Mkdir(root, 0o700))
		require.NoError(t, os.WriteFile(credentialPath, []byte(credentialCanary), 0o600))
		paths := []string{tokensDir, root, filepath.Join(root, ".credentials.lock"), credentialPath}
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential target", func(t *testing.T) {
		tokensDir := t.TempDir()
		require.NoError(t, os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		credentialPath := filepath.Join(root, "profile.json")
		require.NoError(t, os.Mkdir(root, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".credentials.lock"), nil, 0o600))
		paths := []string{tokensDir, root, filepath.Join(root, ".credentials.lock"), credentialPath}
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential tombstone", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		require.NoError(t, os.Truncate(paths[len(paths)-1], 0))
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStoreDeleteRejectsMissingStateAfterPreflightWithoutRecreatingIt(t *testing.T) {
	t.Run("tokens root", func(t *testing.T) {
		parent := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		require.NoError(t, os.Mkdir(tokensDir, 0o700))
		createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		retained := tokensDir + ".retained"
		require.NoError(t, os.Rename(tokensDir, retained))
		before := snapshotCredentialPaths(t,
			parent,
			tokensDir,
			retained,
			filepath.Join(retained, "people-providers"),
			filepath.Join(retained, "people-providers", ".credentials.lock"),
			filepath.Join(retained, "people-providers", "profile.json"),
		)

		err = store.Delete("profile", guard)
		require.Error(t, err)
		if err != nil {
			assert.NotContains(t, err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential namespace", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		root := paths[1]
		retained := root + ".retained"
		require.NoError(t, os.Rename(root, retained))
		before := snapshotCredentialPaths(t,
			tokensDir,
			root,
			retained,
			filepath.Join(retained, ".credentials.lock"),
			filepath.Join(retained, "profile.json"),
		)

		err = store.Delete("profile", guard)
		require.Error(t, err)
		if err != nil {
			assert.NotContains(t, err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("lock marker", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		lockPath := paths[2]
		retained := lockPath + ".retained"
		require.NoError(t, os.Rename(lockPath, retained))
		before := snapshotCredentialPaths(t, paths[0], paths[1], lockPath, retained, paths[3])

		err = store.Delete("profile", guard)
		require.Error(t, err)
		if err != nil {
			assert.NotContains(t, err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential target", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		credentialPath := paths[3]
		retained := credentialPath + ".retained"
		require.NoError(t, os.Rename(credentialPath, retained))
		before := snapshotCredentialPaths(t, paths[0], paths[1], paths[2], credentialPath, retained)

		err = store.Delete("profile", guard)
		require.ErrorContains(t, err, "credential changed")
		if err != nil {
			assert.NotContains(t, err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential tombstone", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		require.NoError(t, os.Truncate(paths[3], 0))
		before := snapshotCredentialPaths(t, paths...)

		err = store.Delete("profile", guard)
		require.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
		if err != nil {
			assert.NotContains(t, err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStorePreflightDeleteRejectsWrongModesWithoutRepair(t *testing.T) {
	for _, test := range []struct {
		name  string
		index int
		mode  os.FileMode
	}{
		{name: "tokens root", index: 0, mode: 0o750},
		{name: "credential namespace", index: 1, mode: 0o750},
		{name: "lock marker", index: 2, mode: 0o640},
		{name: "credential target", index: 3, mode: 0o640},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokensDir := t.TempDir()
			paths := createExistingCredentialPreflightFixture(t, tokensDir)
			require.NoError(t, os.Chmod(paths[test.index], test.mode))
			before := snapshotCredentialPaths(t, paths...)

			err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
			require.ErrorContains(t, err, "permissions")
			if err != nil {
				assert.NotContains(t, err.Error(), credentialCanary)
			}
			assertCredentialPathsUnchanged(t, before)
		})
	}
}

func TestCredentialStoreDeleteRejectsWrongModesAfterPreflightWithoutRepair(t *testing.T) {
	for _, test := range []struct {
		name  string
		index int
		mode  os.FileMode
	}{
		{name: "tokens root", index: 0, mode: 0o750},
		{name: "credential namespace", index: 1, mode: 0o750},
		{name: "lock marker", index: 2, mode: 0o640},
		{name: "credential target", index: 3, mode: 0o640},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokensDir := t.TempDir()
			paths := createExistingCredentialPreflightFixture(t, tokensDir)
			store := peoplesweep.NewFileCredentialStore(tokensDir)
			guard, err := store.PreflightDelete("profile")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, guard.Close()) })

			require.NoError(t, os.Chmod(paths[test.index], test.mode))
			before := snapshotCredentialPaths(t, paths...)

			err = store.Delete("profile", guard)
			require.ErrorContains(t, err, "changed")
			if err != nil {
				assert.NotContains(t, err.Error(), credentialCanary)
			}
			assertCredentialPathsUnchanged(t, before)
		})
	}
}

func TestCredentialStoreDeleteRejectsUnsafeTargetAfterPreflightWithoutChangingIt(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		credentialPath := paths[3]
		retained := credentialPath + ".retained"
		external := filepath.Join(t.TempDir(), "external")
		require.NoError(t, os.Rename(credentialPath, retained))
		require.NoError(t, os.WriteFile(external, []byte("external-must-remain"), 0o600))
		require.NoError(t, os.Symlink(external, credentialPath))
		before := snapshotCredentialPaths(t,
			paths[0], paths[1], paths[2], credentialPath, retained, external,
		)

		err = store.Delete("profile", guard)
		require.ErrorContains(t, err, "changed")
		if err != nil {
			assert.NotContains(t, err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("hard link", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		linkPath := filepath.Join(t.TempDir(), "linked-credential")
		require.NoError(t, os.Link(paths[3], linkPath))
		before := snapshotCredentialPaths(t, paths[0], paths[1], paths[2], paths[3], linkPath)

		err = store.Delete("profile", guard)
		require.ErrorContains(t, err, "changed")
		if err != nil {
			assert.NotContains(t, err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("FIFO", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, guard.Close()) })

		retained := paths[3] + ".retained"
		require.NoError(t, os.Rename(paths[3], retained))
		require.NoError(t, unix.Mkfifo(paths[3], 0o600))
		before := snapshotCredentialPaths(t, paths[0], paths[1], paths[2], paths[3], retained)

		result := make(chan error, 1)
		go func() {
			result <- store.Delete("profile", guard)
		}()
		select {
		case err := <-result:
			require.ErrorContains(t, err, "changed")
			if err != nil {
				assert.NotContains(t, err.Error(), credentialCanary)
			}
		case <-time.After(time.Second):
			assert.Fail(t, "credential deletion blocked while inspecting a FIFO")
		}
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStorePreflightDeleteRejectsUnsafeObjectsWithoutChangingThem(t *testing.T) {
	t.Run("tokens directory symlink", func(t *testing.T) {
		parent := t.TempDir()
		target := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		require.NoError(t, os.Symlink(target, tokensDir))
		before := snapshotCredentialPaths(t, parent, target, tokensDir)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential namespace symlink", func(t *testing.T) {
		tokensDir := t.TempDir()
		require.NoError(t, os.Chmod(tokensDir, 0o700))
		target := t.TempDir()
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(t, os.Symlink(target, root))
		before := snapshotCredentialPaths(t, tokensDir, target, root)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("lock symlink", func(t *testing.T) {
		tokensDir := t.TempDir()
		require.NoError(t, os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(t, os.Mkdir(root, 0o700))
		external := filepath.Join(t.TempDir(), "external-lock")
		require.NoError(t, os.WriteFile(external, nil, 0o600))
		lockPath := filepath.Join(root, ".credentials.lock")
		require.NoError(t, os.Symlink(external, lockPath))
		before := snapshotCredentialPaths(t, tokensDir, root, lockPath, external)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential symlink", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		credentialPath := paths[len(paths)-1]
		require.NoError(t, os.Remove(credentialPath))
		external := filepath.Join(t.TempDir(), "external-credential")
		require.NoError(t, os.WriteFile(external, []byte(credentialCanary), 0o600))
		require.NoError(t, os.Symlink(external, credentialPath))
		paths = append(paths, external)
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), credentialCanary)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential hard link", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		credentialPath := paths[len(paths)-1]
		linkPath := filepath.Join(t.TempDir(), "credential-link")
		require.NoError(t, os.Link(credentialPath, linkPath))
		paths = append(paths, linkPath)
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorContains(t, err, "links")
		assert.NotContains(t, err.Error(), credentialCanary)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential directory", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		credentialPath := paths[len(paths)-1]
		require.NoError(t, os.Remove(credentialPath))
		require.NoError(t, os.Mkdir(credentialPath, 0o700))
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorContains(t, err, "not a regular file")
		assert.NotContains(t, err.Error(), credentialCanary)
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStoreRotatesAtomically(t *testing.T) {
	tokensDir := t.TempDir()
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(t, store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-old")))

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Go(func() {
		for range 200 {
			credential, err := store.Load("rotate")
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			value := credential.Value()
			if value != credentialCanary+"-old" && value != credentialCanary+"-new" {
				select {
				case errCh <- errors.New("reader observed a partial credential"):
				default:
				}
				return
			}
		}
	})
	for range 100 {
		require.NoError(t, store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-new")))
		require.NoError(t, store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-old")))
	}
	wg.Wait()
	select {
	case err := <-errCh:
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
		wg.Go(func() {
			name := fmt.Sprintf("profile-%02d", index)
			errors <- store.Save(name, peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary))
		})
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
		assert.Equal(t, credentialCanary, credential.Value(), "loaded credential differs")
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
			guard, created, err := store.SaveNew("new-profile", peoplesweep.NewCredential(
				peoplesweep.AuthBearer, value,
			))
			if guard != nil {
				err = errors.Join(err, guard.Close())
			}
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
	assert.Equal(t, credentialCanary, stored.Value(), "stored credential differs")

	environment, err := resolver.Resolve("ignored-profile-name", credentialTestProfile(t,
		peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer))
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthBearer, environment.Scheme)
	assert.Equal(t, credentialCanary, environment.Value(), "environment credential differs")

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
