//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func retireExactConfigForMissingRestore(current, before ConfigFile) error {
	authority, err := pinWindowsConfigParent(before.Path)
	if err != nil {
		return err
	}
	defer func() { _ = authority.Release() }()
	parent, err := os.Open(filepath.Dir(before.Path))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	info, err := parent.Stat()
	if err != nil {
		return err
	}
	identity, ok := openedFileIdentity(parent, info)
	if !ok || identity != before.parentIdentity {
		return errors.Join(ErrConfigConflict, errors.New("config rollback parent changed"))
	}
	retained, err := openConfigNoFollow(current.Path)
	if err != nil {
		return err
	}
	defer func() { _ = retained.Close() }()
	if err := retireWindowsConfigArtifact(current.Path, retained); err != nil {
		return fmt.Errorf("retire created config during rollback: %w", err)
	}
	return nil
}
