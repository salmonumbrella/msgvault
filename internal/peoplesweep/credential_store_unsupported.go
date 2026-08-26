//go:build !linux && !darwin

package peoplesweep

import "errors"

var errCredentialStoreUnsupported = errors.New(
	"people provider credential store is unsupported on this platform because secure no-follow atomic filesystem operations are unavailable",
)

func (s *FileCredentialStore) preflightExistingCredentialDelete(profileName string) error {
	_ = s
	_ = profileName
	return errCredentialStoreUnsupported
}

func (s *FileCredentialStore) withCredentialRoot(
	operation string,
	callback func(credentialStoreRoot) error,
) error {
	_ = s
	_ = operation
	_ = callback
	return errCredentialStoreUnsupported
}
