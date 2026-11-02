// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Keep the default wire format compatible with released CAA consumers.
// Legacy consumers may ignore options; this format does not promise guest
// filesystem read-only enforcement or delegated fsGroup support.
type legacyMountInfo struct {
	VolumeType string            `json:"volume-type"`
	Device     string            `json:"device"`
	FsType     string            `json:"fstype"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Options    []string          `json:"options,omitempty"`
}

func publishLegacyMetadata(path string, info directVolumeMetadata) error {
	metadata := legacyMountInfo{
		VolumeType: "directvol", Device: info.Device, FsType: info.FsType,
		Metadata: info.Metadata, Options: info.Options,
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal mountInfo: %v", err)
	}
	existing, err := os.ReadFile(path)
	if err == nil {
		// Compare decoded objects so indentation and object-key ordering do not
		// turn a compatible replay into a conflict. Never overwrite v1 metadata
		// with the legacy format during a live downgrade.
		var previous map[string]interface{}
		if err := json.Unmarshal(existing, &previous); err != nil {
			return status.Errorf(codes.Internal, "invalid existing mountInfo: %v", err)
		}
		if priorMetadata, ok := previous["metadata"].(map[string]interface{}); ok {
			// Earlier compatible writers copied the controller parameters and
			// logical handle into metadata. Compare the fields consumed by CAA
			// without requiring a rewrite of a live legacy record.
			if handle, ok := priorMetadata["cloud-volume-id"].(string); ok && handle != info.VolumeHandle {
				return status.Error(codes.AlreadyExists, "target belongs to a different logical volume")
			}
			values := make(map[string]string)
			for key, value := range priorMetadata {
				if value, ok := value.(string); ok {
					values[key] = value
				}
			}
			if values["cloud-provider"] == "" {
				values["cloud-provider"] = values["cloudProvider"]
			}
			previous["metadata"] = directVolumeMetadataContext(values)
		}
		previousJSON, _ := json.Marshal(previous)
		var desired map[string]interface{}
		_ = json.Unmarshal(data, &desired)
		desiredJSON, _ := json.Marshal(desired)
		if !bytes.Equal(previousJSON, desiredJSON) {
			return status.Error(codes.AlreadyExists, "target is already published with incompatible metadata; unpublish before changing formats")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return status.Errorf(codes.Internal, "read mountInfo: %v", err)
	}
	if err := writeMountInfoAtomic(path, append(data, '\n')); err != nil {
		return status.Errorf(codes.Internal, "publish mountInfo: %v", err)
	}
	return nil
}

// Read both deployed formats during cleanup. Versioned records retain their
// identity checks even when the configured writer has been rolled back.
func readPublishedIdentity(path string) (*directVolumeMetadata, error) {
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
		return nil, fmt.Errorf("mountInfo exceeds size limit")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if _, versioned := fields["version"]; versioned {
		return readDirectVolumeMetadata(path)
	}
	var legacy legacyMountInfo
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		return nil, err
	}
	if (legacy.VolumeType != "block" && legacy.VolumeType != "directvol") || legacy.Device == "" || legacy.FsType == "" {
		return nil, fmt.Errorf("invalid legacy mountInfo")
	}
	return nil, nil
}
