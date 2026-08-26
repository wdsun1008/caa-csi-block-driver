// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
)

const (
	defaultVolumeCapacity     = int64(1 << 30)
	volumeRecordSchemaVersion = 1
	volumeRecordStatePending  = "pending"
	volumeRecordStateReady    = "ready"
	sha256HexLength           = 64
	operationIDHexLength      = 32
)

type volumeNameLock struct {
	mu   sync.Mutex
	refs int
}

type volumeNameLocks struct {
	mu    sync.Mutex
	locks map[string]*volumeNameLock
}

func (locks *volumeNameLocks) lock(name string) func() {
	locks.mu.Lock()
	if locks.locks == nil {
		locks.locks = make(map[string]*volumeNameLock)
	}
	entry := locks.locks[name]
	if entry == nil {
		entry = &volumeNameLock{}
		locks.locks[name] = entry
	}
	entry.refs++
	locks.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.locks, name)
		}
		locks.mu.Unlock()
	}
}

func capacityRange(req *csi.CreateVolumeRequest) (required, limit int64, err error) {
	required = req.GetCapacityRange().GetRequiredBytes()
	limit = req.GetCapacityRange().GetLimitBytes()
	if required < 0 || limit < 0 {
		return 0, 0, fmt.Errorf("capacity values cannot be negative")
	}
	if required == 0 {
		required = defaultVolumeCapacity
	}
	if limit > 0 && required > limit {
		return 0, 0, fmt.Errorf("required capacity %d exceeds limit %d", required, limit)
	}
	return required, limit, nil
}

// createRequestFingerprint binds a durable operation to the canonical CSI
// request and the capacity selected by the provider. The name itself is the
// store key and therefore does not need to be repeated in the digest.
func createRequestFingerprint(req *csi.CreateVolumeRequest, plannedCapacity int64) (string, error) {
	capabilities := make([]string, 0, len(req.GetVolumeCapabilities()))
	for _, capability := range req.GetVolumeCapabilities() {
		encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(protoadapt.MessageV2Of(capability))
		if err != nil {
			return "", fmt.Errorf("marshal volume capability: %w", err)
		}
		capabilities = append(capabilities, hex.EncodeToString(encoded))
	}
	sort.Strings(capabilities)

	sourceType, sourceID := "", ""
	if source := req.GetVolumeContentSource(); source != nil {
		switch {
		case source.GetSnapshot() != nil:
			sourceType, sourceID = "snapshot", source.GetSnapshot().GetSnapshotId()
		case source.GetVolume() != nil:
			sourceType, sourceID = "volume", source.GetVolume().GetVolumeId()
		default:
			return "", fmt.Errorf("unsupported volume content source")
		}
	}

	topology := ""
	if requirements := req.GetAccessibilityRequirements(); requirements != nil {
		encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(protoadapt.MessageV2Of(requirements))
		if err != nil {
			return "", fmt.Errorf("marshal accessibility requirements: %w", err)
		}
		topology = hex.EncodeToString(encoded)
	}

	payload, err := json.Marshal(struct {
		Parameters      map[string]string `json:"parameters"`
		Capabilities    []string          `json:"capabilities"`
		SourceType      string            `json:"sourceType,omitempty"`
		SourceID        string            `json:"sourceID,omitempty"`
		Topology        string            `json:"topology,omitempty"`
		PlannedCapacity int64             `json:"plannedCapacity"`
	}{
		Parameters: copyStringMap(req.GetParameters()), Capabilities: capabilities,
		SourceType: sourceType, SourceID: sourceID, Topology: topology,
		PlannedCapacity: plannedCapacity,
	})
	if err != nil {
		return "", fmt.Errorf("marshal create request: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func newVolumeOperationID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate create operation ID: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func isHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func requestFitsRecord(rec *volumeRecord, fingerprint string, required, limit int64) error {
	if rec.RequestFingerprint != fingerprint {
		return fmt.Errorf("parameters, capabilities, content source, topology, or planned capacity differ")
	}
	capacity := rec.CapacityBytes
	if rec.State == volumeRecordStatePending {
		capacity = rec.PlannedCapacityBytes
	}
	if capacity < required {
		return fmt.Errorf("volume capacity %d is below required %d", capacity, required)
	}
	if limit > 0 && capacity > limit {
		return fmt.Errorf("volume capacity %d exceeds limit %d", capacity, limit)
	}
	return nil
}

func legacyRecordCompatible(rec *volumeRecord, params map[string]string, required, limit int64) error {
	if !reflect.DeepEqual(rec.Params, params) {
		return fmt.Errorf("parameters differ")
	}
	if rec.CapacityBytes < required {
		return fmt.Errorf("volume capacity %d is below required %d", rec.CapacityBytes, required)
	}
	if limit > 0 && rec.CapacityBytes > limit {
		return fmt.Errorf("volume capacity %d exceeds limit %d", rec.CapacityBytes, limit)
	}
	return nil
}

func copyStringMap(values map[string]string) map[string]string {
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
