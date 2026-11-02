// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"fmt"
	"os"
	"path/filepath"
)

func writeMountInfoAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create direct-volume directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".mountInfo.json.*")
	if err != nil {
		return fmt.Errorf("create direct-volume temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("chmod direct-volume temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write direct-volume temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync direct-volume temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close direct-volume temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish direct-volume metadata: %w", err)
	}

	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open direct-volume directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync direct-volume directory: %w", err)
	}
	return nil
}
