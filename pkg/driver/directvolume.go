// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	directVolumeMetadataVersion = 1
	directVolumeModeFilesystem  = "filesystem"
	maxDirectVolumeMetadataSize = 64 * 1024
)

// directVolumeMetadata is the versioned contract written by the CSI node
// plugin and consumed by Cloud API Adaptor. It deliberately keeps readonly as
// an explicit field: mount options are retained for the filesystem mount, but
// are not the security contract.
type directVolumeMetadata struct {
	Version      int               `json:"version"`
	VolumeType   string            `json:"volume-type"`
	VolumeMode   string            `json:"volume-mode"`
	Device       string            `json:"device"`
	FsType       string            `json:"fstype"`
	ReadOnly     bool              `json:"readonly"`
	Options      []string          `json:"options,omitempty"`
	FSGroup      string            `json:"fs-group,omitempty"`
	PodUID       string            `json:"pod-uid"`
	VolumeHandle string            `json:"volume-handle"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// directVolumeMetadataWire preserves the presence of readonly while decoding
// the untrusted JSON contract. The explicit field shadows the embedded bool,
// so the document is decoded once and then normalized into the internal type.
type directVolumeMetadataWire struct {
	directVolumeMetadata
	ReadOnly *bool `json:"readonly"`
}

func (wire directVolumeMetadataWire) normalize() (*directVolumeMetadata, error) {
	if wire.ReadOnly == nil {
		return nil, errors.New("direct-volume metadata has no explicit readonly field")
	}
	metadata := wire.directVolumeMetadata
	metadata.ReadOnly = *wire.ReadOnly
	return &metadata, nil
}

func normalizeFilesystemType(fsType string) (string, error) {
	fsType = strings.ToLower(strings.TrimSpace(fsType))
	if fsType == "" {
		fsType = "ext4"
	}
	switch fsType {
	case "ext4", "ext3", "xfs":
		return fsType, nil
	default:
		return "", fmt.Errorf("filesystem type %q is not supported", fsType)
	}
}

func normalizeFSGroup(fsGroup string) (string, error) {
	fsGroup = strings.TrimSpace(fsGroup)
	if fsGroup == "" {
		return "", nil
	}
	value, err := strconv.ParseUint(fsGroup, 10, 32)
	if err != nil || strconv.FormatUint(value, 10) != fsGroup {
		return "", fmt.Errorf("invalid volume mount group %q", fsGroup)
	}
	return fsGroup, nil
}

func normalizeMountOptions(flags []string, readOnly bool) ([]string, bool, error) {
	allowed := map[string]bool{
		"async": true, "atime": true, "defaults": true, "diratime": true,
		"dev": true, "dirsync": true, "discard": true, "exec": true,
		"lazytime": true, "noatime": true, "nodev": true, "nodiratime": true,
		"noexec": true, "nosuid": true,
		"relatime": true, "ro": true, "rw": true, "strictatime": true,
		"suid": true, "sync": true,
	}
	conflictGroup := map[string]string{
		"ro": "access", "rw": "access",
		"sync": "io", "async": "io",
		"atime": "atime", "noatime": "atime", "relatime": "atime", "strictatime": "atime",
		"diratime": "diratime", "nodiratime": "diratime",
		"exec": "exec", "noexec": "exec",
		"dev": "dev", "nodev": "dev",
		"suid": "suid", "nosuid": "suid",
	}
	seen := make(map[string]struct{}, len(flags)+1)
	selected := make(map[string]string)
	options := make([]string, 0, len(flags)+1)
	for _, raw := range flags {
		option := strings.TrimSpace(strings.ToLower(raw))
		if option == "" {
			continue
		}
		if strings.ContainsAny(option, " ,\t\r\n") || !allowed[option] {
			return nil, false, fmt.Errorf("mount option %q is not allowed", raw)
		}
		if group := conflictGroup[option]; group != "" {
			if previous := selected[group]; previous != "" && previous != option {
				return nil, false, fmt.Errorf("mount options %q and %q conflict", previous, option)
			}
			selected[group] = option
		}
		if option == "ro" {
			readOnly = true
		}
		if _, ok := seen[option]; ok {
			continue
		}
		seen[option] = struct{}{}
		options = append(options, option)
	}
	if readOnly {
		if _, ok := seen["rw"]; ok {
			return nil, false, fmt.Errorf("readonly volume conflicts with mount option %q", "rw")
		}
		if _, ok := seen["ro"]; !ok {
			options = append(options, "ro")
		}
	}
	sort.Strings(options)
	return options, readOnly, nil
}

// directVolumeMetadataContext is the only provider metadata exposed across
// the host/guest boundary. Controller parameters remain in the durable record
// for idempotency and provider reconstruction, but are not copied wholesale
// into mountInfo.json.
func directVolumeMetadataContext(values map[string]string) map[string]string {
	result := make(map[string]string, 4)
	for _, key := range []string{
		"cloud-provider", "cloud-volume-path",
		// Preserve the existing upstream CDH contract. These are key
		// references, never key material.
		"encrypt-type", "kbs-key-id",
	} {
		if value := strings.TrimSpace(values[key]); value != "" {
			result[key] = value
		}
	}
	return result
}

func (metadata *directVolumeMetadata) validate() error {
	if metadata.Version != directVolumeMetadataVersion || metadata.VolumeType != "block" ||
		metadata.VolumeMode != directVolumeModeFilesystem {
		return errors.New("direct-volume metadata has an invalid v1 schema")
	}
	for name, value := range map[string]string{
		"device": metadata.Device, "pod-uid": metadata.PodUID, "volume-handle": metadata.VolumeHandle,
	} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("direct-volume metadata has an invalid %s", name)
		}
	}
	fsType, err := normalizeFilesystemType(metadata.FsType)
	if err != nil || fsType != metadata.FsType {
		return errors.New("direct-volume metadata has an invalid filesystem type")
	}
	options, readOnly, err := normalizeMountOptions(metadata.Options, metadata.ReadOnly)
	if err != nil || readOnly != metadata.ReadOnly || !slices.Equal(options, metadata.Options) {
		return errors.New("direct-volume metadata has invalid or non-canonical mount options")
	}
	fsGroup, err := normalizeFSGroup(metadata.FSGroup)
	if err != nil || fsGroup != metadata.FSGroup {
		return errors.New("direct-volume metadata has an invalid fs-group")
	}
	for key, value := range metadata.Metadata {
		if key != "cloud-provider" && key != "cloud-volume-path" &&
			key != "encrypt-type" && key != "kbs-key-id" {
			return fmt.Errorf("direct-volume metadata contains unsupported metadata key %q", key)
		}
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("direct-volume metadata contains invalid metadata value for %q", key)
		}
	}
	encryptType, keyID := metadata.Metadata["encrypt-type"], metadata.Metadata["kbs-key-id"]
	if (encryptType == "") != (keyID == "") {
		return errors.New("direct-volume metadata must set encrypt-type and kbs-key-id together")
	}
	if encryptType != "" && !strings.EqualFold(encryptType, "luks") &&
		!strings.EqualFold(encryptType, "luks2") {
		return fmt.Errorf("direct-volume metadata has unsupported encrypt-type %q", encryptType)
	}
	return nil
}

func readDirectVolumeMetadata(path string) (*directVolumeMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxDirectVolumeMetadataSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDirectVolumeMetadataSize {
		return nil, fmt.Errorf("direct-volume metadata exceeds %d bytes", maxDirectVolumeMetadataSize)
	}
	var wire directVolumeMetadataWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected trailing JSON value")
		}
		return nil, err
	}
	metadata, err := wire.normalize()
	if err != nil {
		return nil, err
	}
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	return metadata, nil
}

func directVolumeMetadataEqual(left, right directVolumeMetadata) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func writeDirectVolumeMetadataAtomic(path string, metadata directVolumeMetadata) error {
	if err := metadata.validate(); err != nil {
		return fmt.Errorf("validate direct-volume metadata: %w", err)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal direct-volume metadata: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxDirectVolumeMetadataSize {
		return fmt.Errorf("direct-volume metadata exceeds %d bytes", maxDirectVolumeMetadataSize)
	}

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
