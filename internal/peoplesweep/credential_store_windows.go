//go:build windows

package peoplesweep

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/sys/windows"
)

type windowsCredentialStoreRoot struct {
	root  *os.Root
	hooks *credentialStoreHooks
}

func (s *FileCredentialStore) withCredentialRoot(
	operation string,
	callback func(credentialStoreRoot) error,
) (retErr error) {
	if s == nil || filepath.Clean(s.tokensDir) == "." || s.tokensDir == "" {
		return errors.New("people provider credential tokens directory is required")
	}
	if err := fileutil.SecureMkdirAll(s.tokensDir, 0o700); err != nil {
		return fmt.Errorf("create people provider tokens directory: %w", err)
	}
	tokensInfo, err := os.Lstat(s.tokensDir)
	if err != nil || tokensInfo.Mode()&os.ModeSymlink != 0 || !tokensInfo.IsDir() {
		return errors.New("people provider tokens path is not a direct directory")
	}
	tokensRoot, err := os.OpenRoot(s.tokensDir)
	if err != nil {
		return fmt.Errorf("pin people provider tokens directory: %w", err)
	}
	defer tokensRoot.Close()
	if _, err := tokensRoot.Lstat(credentialNamespace); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect people provider credential directory: %w", err)
		}
		if err := tokensRoot.Mkdir(credentialNamespace, 0o700); err != nil {
			return fmt.Errorf("create people provider credential directory: %w", err)
		}
	}
	rootInfo, err := tokensRoot.Lstat(credentialNamespace)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("people provider credential path is not a direct directory")
	}
	root, err := tokensRoot.OpenRoot(credentialNamespace)
	if err != nil {
		return fmt.Errorf("pin people provider credential directory: %w", err)
	}
	defer root.Close()
	lock, err := root.OpenFile(".credentials.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open people provider credential lock in pinned directory: %w", err)
	}
	defer lock.Close()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return errors.New("people provider credential lock is not a regular file")
	}
	if err := lock.Chmod(0o600); err != nil {
		return fmt.Errorf("protect opened people provider credential lock: %w", err)
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("lock opened people provider credential store: %w", err)
	}
	defer func() {
		if err := windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, &overlapped); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock opened people provider credential store: %w", err)
		}
	}()
	if s.hooks != nil && s.hooks.afterLockAcquired != nil {
		s.hooks.afterLockAcquired()
	}
	if s.hooks != nil && s.hooks.beforeOperation != nil {
		s.hooks.beforeOperation(operation)
	}
	return callback(&windowsCredentialStoreRoot{root: root, hooks: s.hooks})
}

func (r *windowsCredentialStoreRoot) save(profileName string, data []byte) error {
	target := profileName + ".json"
	if info, err := r.root.Lstat(target); err == nil &&
		(info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("people provider credential path is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect people provider credential path: %w", err)
	}
	name, candidate, err := createWindowsCredentialCandidate(r.root)
	if err != nil {
		return err
	}
	openedCandidate, err := candidate.Stat()
	if err != nil {
		_ = candidate.Close()
		_ = r.root.Remove(name)
		return fmt.Errorf("identify opened people provider credential candidate: %w", err)
	}
	published := false
	defer func() {
		_ = candidate.Close()
		if !published {
			_ = r.root.Remove(name)
		}
	}()
	if r.hooks != nil && r.hooks.afterCandidateOpen != nil {
		r.hooks.afterCandidateOpen(name)
	}
	if _, err := candidate.Write(data); err != nil {
		return fmt.Errorf("write opened people provider credential candidate: %w", err)
	}
	if err := candidate.Sync(); err != nil {
		return fmt.Errorf("sync opened people provider credential candidate: %w", err)
	}
	if err := candidate.Close(); err != nil {
		return fmt.Errorf("close opened people provider credential candidate: %w", err)
	}
	currentCandidate, err := r.root.Lstat(name)
	if err != nil || !os.SameFile(openedCandidate, currentCandidate) ||
		!currentCandidate.Mode().IsRegular() {
		return errors.New("people provider credential candidate changed before publication")
	}
	if err := r.root.Rename(name, target); err != nil {
		return fmt.Errorf("publish people provider credential in pinned directory: %w", err)
	}
	published = true
	return nil
}

func createWindowsCredentialCandidate(root *os.Root) (string, *os.File, error) {
	for range 128 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, fmt.Errorf("generate people provider credential candidate name: %w", err)
		}
		name := ".credential-" + hex.EncodeToString(random[:]) + ".tmp"
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("create people provider credential candidate: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			_ = root.Remove(name)
			return "", nil, fmt.Errorf("protect opened people provider credential candidate: %w", err)
		}
		return name, file, nil
	}
	return "", nil, errors.New("could not allocate a people provider credential candidate")
}

func (r *windowsCredentialStoreRoot) load(profileName string) ([]byte, error) {
	name := profileName + ".json"
	info, err := r.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("people provider credential path is not a regular file")
	}
	file, err := r.root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open people provider credential in pinned directory: %w", err)
	}
	defer file.Close()
	if r.hooks != nil && r.hooks.afterCredentialOpen != nil {
		r.hooks.afterCredentialOpen("load")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read opened people provider credential: %w", err)
	}
	return data, nil
}

func (r *windowsCredentialStoreRoot) delete(profileName string) error {
	name := profileName + ".json"
	info, err := r.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("people provider credential path is not a regular file")
	}
	file, err := r.root.Open(name)
	if err != nil {
		return fmt.Errorf("open people provider credential for deletion: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("identify opened people provider credential for deletion: %w", err)
	}
	if r.hooks != nil && r.hooks.afterCredentialOpen != nil {
		r.hooks.afterCredentialOpen("delete")
	}
	current, err := r.root.Stat(name)
	if err != nil || !os.SameFile(opened, current) {
		return errors.New("people provider credential changed before deletion")
	}
	if err := r.root.Remove(name); err != nil {
		return fmt.Errorf("remove people provider credential from pinned directory: %w", err)
	}
	return nil
}
