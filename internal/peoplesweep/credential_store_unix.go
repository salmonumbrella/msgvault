//go:build linux || darwin

package peoplesweep

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/sys/unix"
)

// The pinned namespace lock serializes reuse of this single candidate slot.
const unixCredentialCandidateName = ".credential-candidate"

type unixCredentialStoreRoot struct {
	fd    int
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
	tokensFD, err := unix.Open(s.tokensDir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("pin people provider tokens directory without following symlinks: %w", err)
	}
	defer func() {
		if err := unix.Close(tokensFD); err != nil && retErr == nil {
			retErr = fmt.Errorf("close people provider tokens directory: %w", err)
		}
	}()
	if err := validateUnixDirectoryFD(tokensFD, "tokens"); err != nil {
		return err
	}
	if err := unix.Fchmod(tokensFD, 0o700); err != nil {
		return fmt.Errorf("protect pinned people provider tokens directory: %w", err)
	}

	rootFD, created, err := openUnixCredentialNamespace(tokensFD)
	if err != nil {
		return err
	}
	defer func() {
		if err := unix.Close(rootFD); err != nil && retErr == nil {
			retErr = fmt.Errorf("close pinned people provider credential directory: %w", err)
		}
	}()
	if created {
		if err := unix.Fsync(tokensFD); err != nil {
			return fmt.Errorf("sync pinned people provider tokens directory: %w", err)
		}
		if s.hooks != nil && s.hooks.afterNamespaceParentSync != nil {
			s.hooks.afterNamespaceParentSync()
		}
	}
	if err := unix.Flock(rootFD, unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock pinned people provider credential directory: %w", err)
	}
	defer func() {
		if err := unix.Flock(rootFD, unix.LOCK_UN); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock pinned people provider credential directory: %w", err)
		}
	}()
	if err := ensureUnixCredentialLockMarker(rootFD); err != nil {
		return err
	}
	if s.hooks != nil && s.hooks.afterLockAcquired != nil {
		s.hooks.afterLockAcquired()
	}
	if s.hooks != nil && s.hooks.beforeOperation != nil {
		s.hooks.beforeOperation(operation)
	}
	return callback(&unixCredentialStoreRoot{fd: rootFD, hooks: s.hooks})
}

func openUnixCredentialNamespace(tokensFD int) (int, bool, error) {
	rootFD, err := unix.Openat(tokensFD, credentialNamespace,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	created := false
	if err == unix.ENOENT {
		if err := unix.Mkdirat(tokensFD, credentialNamespace, 0o700); err != nil && err != unix.EEXIST {
			return -1, false, fmt.Errorf("create people provider credential directory relative to pinned parent: %w", err)
		}
		created = true
		rootFD, err = unix.Openat(tokensFD, credentialNamespace,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return -1, false, fmt.Errorf("pin people provider credential directory without following symlinks: %w", err)
	}
	if err := validateUnixDirectoryFD(rootFD, "credential"); err != nil {
		_ = unix.Close(rootFD)
		return -1, false, err
	}
	if err := unix.Fchmod(rootFD, 0o700); err != nil {
		_ = unix.Close(rootFD)
		return -1, false, fmt.Errorf("protect pinned people provider credential directory: %w", err)
	}
	return rootFD, created, nil
}

func validateUnixDirectoryFD(fd int, kind string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("identify pinned people provider %s directory: %w", kind, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("people provider %s path is not a direct directory", kind)
	}
	return nil
}

func ensureUnixCredentialLockMarker(rootFD int) error {
	fd, err := unix.Openat(rootFD, ".credentials.lock",
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return fmt.Errorf("open people provider credential lock relative to pinned directory: %w", err)
	}
	defer unix.Close(fd)
	if err := validateUnixCredentialFD(fd, "lock"); err != nil {
		return err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fmt.Errorf("protect opened people provider credential lock: %w", err)
	}
	return nil
}

func (r *unixCredentialStoreRoot) save(profileName string, data []byte) (retErr error) {
	targetName := profileName + ".json"
	if err := inspectUnixCredentialEntry(r.fd, targetName, true); err != nil {
		return err
	}
	candidateName, candidate, err := createUnixCredentialCandidate(r.fd)
	if err != nil {
		return err
	}
	var candidateIdentity unix.Stat_t
	if err := unix.Fstat(int(candidate.Fd()), &candidateIdentity); err != nil {
		_ = candidate.Close()
		return fmt.Errorf("identify opened people provider credential candidate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			retErr = errors.Join(retErr, wipeFailedUnixCredentialCandidate(candidate, r.hooks))
			if err := candidate.Close(); err != nil {
				retErr = errors.Join(retErr, errors.New("close wiped people provider credential candidate failed"))
			}
			return
		}
		_ = candidate.Close()
	}()
	if r.hooks != nil && r.hooks.afterCandidateOpen != nil {
		r.hooks.afterCandidateOpen(candidateName)
	}
	if err := validateUnixCredentialFD(int(candidate.Fd()), "candidate"); err != nil {
		return err
	}
	if _, err := candidate.Write(data); err != nil {
		return fmt.Errorf("write pinned people provider credential candidate: %w", err)
	}
	if err := candidate.Sync(); err != nil {
		return fmt.Errorf("sync pinned people provider credential candidate: %w", err)
	}
	if err := inspectUnixCredentialEntry(r.fd, targetName, true); err != nil {
		return err
	}
	var currentCandidate unix.Stat_t
	if err := unix.Fstatat(r.fd, candidateName, &currentCandidate, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		candidateIdentity.Dev != currentCandidate.Dev || candidateIdentity.Ino != currentCandidate.Ino ||
		validateUnixCredentialStat(currentCandidate, "candidate") != nil {
		return errors.New("people provider credential candidate changed before publication")
	}
	if r.hooks != nil && r.hooks.beforeCandidatePublish != nil {
		r.hooks.beforeCandidatePublish(candidateName)
	}
	if err := unix.Renameat(r.fd, candidateName, r.fd, targetName); err != nil {
		return fmt.Errorf("publish people provider credential relative to pinned directory: %w", err)
	}
	publishedFD, err := unix.Openat(r.fd, targetName,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("people provider credential candidate changed during publication")
	}
	defer unix.Close(publishedFD)
	var publishedIdentity unix.Stat_t
	if err := unix.Fstat(publishedFD, &publishedIdentity); err != nil ||
		candidateIdentity.Dev != publishedIdentity.Dev || candidateIdentity.Ino != publishedIdentity.Ino ||
		validateUnixCredentialStat(publishedIdentity, "candidate") != nil {
		return errors.New("people provider credential candidate changed during publication")
	}
	if err := unix.Fsync(r.fd); err != nil {
		return fmt.Errorf("sync pinned people provider credential directory: %w", err)
	}
	committed = true
	return nil
}

func createUnixCredentialCandidate(rootFD int) (string, *os.File, error) {
	for range 2 {
		created := false
		fd, err := unix.Openat(rootFD, unixCredentialCandidateName,
			unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err == unix.ENOENT {
			fd, err = unix.Openat(rootFD, unixCredentialCandidateName,
				unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
			if err == unix.EEXIST {
				continue
			}
			created = err == nil
		}
		if err != nil {
			return "", nil, fmt.Errorf("open people provider credential candidate relative to pinned directory: %w", err)
		}
		if err := validateUnixCredentialFD(fd, "candidate"); err != nil {
			_ = unix.Close(fd)
			return "", nil, err
		}
		if !created {
			if err := unix.Ftruncate(fd, 0); err != nil {
				_ = unix.Close(fd)
				return "", nil, errors.New("reset pinned people provider credential candidate")
			}
			if err := unix.Fsync(fd); err != nil {
				_ = unix.Close(fd)
				return "", nil, errors.New("sync reset people provider credential candidate")
			}
		}
		return unixCredentialCandidateName, os.NewFile(uintptr(fd), unixCredentialCandidateName), nil
	}
	return "", nil, errors.New("could not open the people provider credential candidate")
}

func (r *unixCredentialStoreRoot) load(profileName string) ([]byte, error) {
	name := profileName + ".json"
	fd, err := unix.Openat(r.fd, name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == unix.ENOENT {
		return nil, fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	if err != nil {
		return nil, fmt.Errorf("open people provider credential for profile %q without following symlinks: %w", profileName, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := validateUnixCredentialFD(fd, "file"); err != nil {
		return nil, err
	}
	if r.hooks != nil && r.hooks.afterCredentialOpen != nil {
		r.hooks.afterCredentialOpen("load")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read pinned people provider credential for profile %q: %w", profileName, err)
	}
	if len(data) == 0 {
		// Delete leaves a durable empty record so it never has to unlink an
		// attacker-replaceable pathname.
		return nil, fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	return data, nil
}

func (r *unixCredentialStoreRoot) preflightDelete(profileName string) error {
	name := profileName + ".json"
	fd, err := unix.Openat(r.fd, name,
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == unix.ENOENT {
		return nil
	}
	if err == unix.ELOOP {
		return errors.New("people provider credential path is not a regular file")
	}
	if err != nil {
		return fmt.Errorf("open people provider credential for deletion preflight without following symlinks: %w", err)
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return fmt.Errorf("identify opened people provider credential for deletion preflight: %w", err)
	}
	if err := validateUnixCredentialStat(opened, "file"); err != nil {
		return err
	}
	if r.hooks != nil && r.hooks.afterCredentialOpen != nil {
		r.hooks.afterCredentialOpen("preflight-delete")
	}
	if !unixCredentialEntryMatches(r.fd, name, opened) {
		return errors.New("people provider credential changed during deletion preflight")
	}
	return nil
}

func (r *unixCredentialStoreRoot) delete(profileName string) error {
	name := profileName + ".json"
	fd, err := unix.Openat(r.fd, name,
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == unix.ENOENT {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open people provider credential for deletion without following symlinks: %w", err)
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return fmt.Errorf("identify opened people provider credential for deletion: %w", err)
	}
	if err := validateUnixCredentialStat(opened, "file"); err != nil {
		return err
	}
	if r.hooks != nil && r.hooks.afterCredentialOpen != nil {
		r.hooks.afterCredentialOpen("delete")
	}
	if opened.Size == 0 {
		// A zero-length record is the bounded, idempotent deletion tombstone.
		if !unixCredentialEntryMatches(r.fd, name, opened) {
			return errors.New("people provider credential changed during deletion")
		}
		return nil
	}
	if !unixCredentialEntryMatches(r.fd, name, opened) {
		return errors.New("people provider credential changed before deletion")
	}
	if r.hooks != nil && r.hooks.beforeCredentialRetire != nil {
		r.hooks.beforeCredentialRetire()
	}
	if err := validateUnixCredentialFD(fd, "file"); err != nil {
		return errors.New("people provider credential changed before deletion")
	}
	wipeErr := wipeUnixCredentialFD(fd)
	if !unixCredentialEntryMatches(r.fd, name, opened) {
		return errors.New("people provider credential changed during deletion")
	}
	if wipeErr != nil {
		return wipeErr
	}
	return nil
}

func inspectUnixCredentialEntry(rootFD int, name string, allowMissing bool) error {
	fd, err := unix.Openat(rootFD, name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == unix.ENOENT && allowMissing {
		return nil
	}
	if err == unix.ELOOP {
		return errors.New("people provider credential path is not a regular file")
	}
	if err != nil {
		return fmt.Errorf("inspect people provider credential entry without following symlinks: %w", err)
	}
	defer unix.Close(fd)
	return validateUnixCredentialFD(fd, "file")
}

func wipeUnixCredentialFD(fd int) error {
	if err := unix.Ftruncate(fd, 0); err != nil {
		return fmt.Errorf("wipe pinned people provider credential during deletion: %w", err)
	}
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("sync wiped people provider credential during deletion: %w", err)
	}
	return nil
}

func wipeFailedUnixCredentialCandidate(candidate *os.File, hooks *credentialStoreHooks) error {
	if err := validateUnixCredentialFD(int(candidate.Fd()), "candidate"); err != nil {
		return errors.New("people provider credential candidate wipe refused")
	}
	var truncateErr error
	if hooks != nil && hooks.failedCandidateTruncate != nil {
		truncateErr = hooks.failedCandidateTruncate()
	} else {
		truncateErr = candidate.Truncate(0)
	}
	var syncErr error
	if hooks != nil && hooks.failedCandidateSync != nil {
		syncErr = hooks.failedCandidateSync()
	} else {
		syncErr = candidate.Sync()
	}
	var cleanupErrors []error
	if truncateErr != nil {
		cleanupErrors = append(cleanupErrors,
			errors.New("people provider credential candidate wipe failed"))
	}
	if syncErr != nil {
		cleanupErrors = append(cleanupErrors,
			errors.New("people provider credential candidate durable wipe failed"))
	}
	return errors.Join(cleanupErrors...)
}

func validateUnixCredentialFD(fd int, kind string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("identify opened people provider credential %s: %w", kind, err)
	}
	return validateUnixCredentialStat(stat, kind)
}

func validateUnixCredentialStat(stat unix.Stat_t, kind string) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("people provider credential %s is not a regular file", kind)
	}
	if stat.Mode&0o777 != 0o600 {
		return fmt.Errorf("people provider credential %s permissions are not private", kind)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("people provider credential %s owner does not match the current user", kind)
	}
	if uint64(stat.Nlink) != 1 {
		return fmt.Errorf("people provider credential %s has an unsafe number of links", kind)
	}
	return nil
}

func unixCredentialEntryMatches(rootFD int, name string, opened unix.Stat_t) bool {
	var current unix.Stat_t
	if err := unix.Fstatat(rootFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return opened.Dev == current.Dev && opened.Ino == current.Ino &&
		validateUnixCredentialStat(current, "file") == nil
}
