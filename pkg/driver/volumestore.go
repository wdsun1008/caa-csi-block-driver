// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var vsLogger = log.New(log.Writer(), "[caa-csi/store] ", log.LstdFlags|log.Lmsgprefix)

const (
	defaultVolumeStoreDir = "/var/lib/caa-csi-block/volumes"
	manifestFileName      = "_manifest.json"
	bootstrapParamsEnv    = "CSI_BOOTSTRAP_PARAMS_FILE"
)

type volumeRecord struct {
	SchemaVersion        int               `json:"schemaVersion,omitempty"`
	State                string            `json:"state,omitempty"`
	VolumeID             string            `json:"volumeID"`
	Provider             string            `json:"provider"`
	Path                 string            `json:"path,omitempty"`
	PlannedCapacityBytes int64             `json:"plannedCapacityBytes,omitempty"`
	CapacityBytes        int64             `json:"capacityBytes,omitempty"`
	Params               map[string]string `json:"params"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	RequestFingerprint   string            `json:"requestFingerprint,omitempty"`
	OperationID          string            `json:"operationID,omitempty"`
}

// volumeManifest keeps only non-secret provider bootstrap parameters so a
// controller can reconcile cloud-tagged volumes after loss of one record.
type volumeManifest struct {
	Params    map[string]string `json:"params"`
	VolumeIDs []string          `json:"volumeIDs"`
}

type volumeStore struct {
	mu  sync.Mutex
	dir string
}

func newVolumeStore() (*volumeStore, error) {
	dir := os.Getenv("CSI_VOLUME_STORE_DIR")
	if dir == "" {
		dir = defaultVolumeStoreDir
	}
	return newVolumeStoreAt(dir)
}

func newVolumeStoreAt(dir string) (*volumeStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create volume store: %w", err)
	}
	return &volumeStore{dir: filepath.Clean(dir)}, nil
}

func validateVolumeStoreKey(volumeID string) error {
	if volumeID == "" || strings.TrimSpace(volumeID) != volumeID || len(volumeID) > 253 ||
		filepath.Base(volumeID) != volumeID || volumeID == "." || volumeID == ".." || strings.Contains(volumeID, `\`) {
		return fmt.Errorf("invalid volume store key %q", volumeID)
	}
	return nil
}

func (vs *volumeStore) path(volumeID string) (string, error) {
	if err := validateVolumeStoreKey(volumeID); err != nil {
		return "", err
	}
	return filepath.Join(vs.dir, volumeID+".json"), nil
}

func (vs *volumeStore) manifestPath() string {
	return filepath.Join(vs.dir, manifestFileName)
}

func validateVolumeRecord(rec *volumeRecord) error {
	if rec == nil {
		return errors.New("volume record is nil")
	}
	if err := validateVolumeStoreKey(rec.VolumeID); err != nil {
		return err
	}
	if strings.TrimSpace(rec.Provider) == "" || strings.TrimSpace(rec.Provider) != rec.Provider {
		return errors.New("volume record has an invalid provider")
	}
	if rec.Params == nil {
		return errors.New("volume record has no provider parameters")
	}

	// Schema zero is the upstream recovery/legacy ready-record shape. It is
	// readable but never used for a new cloud mutation.
	if rec.SchemaVersion == 0 {
		if rec.State != "" || strings.TrimSpace(rec.Path) == "" || strings.TrimSpace(rec.Path) != rec.Path {
			return errors.New("legacy volume record has an invalid cloud volume path")
		}
		if rec.CapacityBytes <= 0 {
			return errors.New("legacy volume record has an invalid capacity")
		}
		return nil
	}
	if rec.SchemaVersion != volumeRecordSchemaVersion {
		return fmt.Errorf("unsupported volume record schema version %d", rec.SchemaVersion)
	}
	if len(rec.RequestFingerprint) != sha256HexLength || !isHex(rec.RequestFingerprint) {
		return errors.New("volume record has an invalid request fingerprint")
	}
	if rec.PlannedCapacityBytes <= 0 {
		return errors.New("volume record has an invalid planned capacity")
	}

	switch rec.State {
	case volumeRecordStatePending:
		if len(rec.OperationID) != operationIDHexLength || !isHex(rec.OperationID) {
			return errors.New("pending volume record has an invalid operation ID")
		}
		if rec.Path != "" || rec.CapacityBytes != 0 {
			return errors.New("pending volume record contains ready state")
		}
	case volumeRecordStateReady:
		if rec.OperationID != "" && (len(rec.OperationID) != operationIDHexLength || !isHex(rec.OperationID)) {
			return errors.New("ready volume record has an invalid operation ID")
		}
		if strings.TrimSpace(rec.Path) == "" || strings.TrimSpace(rec.Path) != rec.Path {
			return errors.New("ready volume record has an invalid cloud volume path")
		}
		if rec.CapacityBytes <= 0 {
			return errors.New("ready volume record has an invalid capacity")
		}
	default:
		return fmt.Errorf("volume record has invalid state %q", rec.State)
	}
	return nil
}

func (vs *volumeStore) Exists(volumeID string) (bool, error) {
	_, err := vs.Load(volumeID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (vs *volumeStore) Save(rec *volumeRecord) error {
	if err := validateVolumeRecord(rec); err != nil {
		return fmt.Errorf("refuse to persist invalid volume record: %w", err)
	}
	path, err := vs.path(rec.VolumeID)
	if err != nil {
		return err
	}

	vs.mu.Lock()
	defer vs.mu.Unlock()
	if err := writeJSONAtomic(path, rec); err != nil {
		return fmt.Errorf("persist volume record: %w", err)
	}
	return vs.writeManifestLocked()
}

func (vs *volumeStore) Load(volumeID string) (*volumeRecord, error) {
	path, err := vs.path(volumeID)
	if err != nil {
		return nil, err
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return loadVolumeRecord(path, volumeID)
}

func loadVolumeRecord(path, expectedVolumeID string) (*volumeRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var rec volumeRecord
	if err := json.NewDecoder(file).Decode(&rec); err != nil {
		return nil, fmt.Errorf("decode volume record: %w", err)
	}
	if rec.VolumeID != expectedVolumeID {
		return nil, fmt.Errorf("volume record identity %q does not match key %q", rec.VolumeID, expectedVolumeID)
	}
	if err := validateVolumeRecord(&rec); err != nil {
		return nil, fmt.Errorf("invalid volume record: %w", err)
	}
	return &rec, nil
}

func (vs *volumeStore) Delete(volumeID string) error {
	path, err := vs.path(volumeID)
	if err != nil {
		return err
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove volume record: %w", err)
	}
	if err := syncDirectory(vs.dir); err != nil {
		return fmt.Errorf("sync volume store directory: %w", err)
	}
	return vs.writeManifestLocked()
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".volume-store-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// BootstrapParams resolves non-secret provider parameters for recovery.
func (vs *volumeStore) BootstrapParams() map[string]string {
	if params := vs.AnyParams(); params != nil {
		return params
	}
	if params := vs.loadManifestParams(); params != nil {
		return params
	}
	if params := loadBootstrapParamsFile(); params != nil {
		return params
	}
	return paramsFromEnv()
}

// RecoverFromCloud rebuilds legacy ready records for volumes already tagged by
// a provider. It never starts a cloud mutation and never overwrites pending
// operation identity.
func (vs *volumeStore) RecoverFromCloud(ctx context.Context, params map[string]string) error {
	if params == nil || params["cloudProvider"] == "" {
		return nil
	}
	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return fmt.Errorf("creating provider for recovery: %w", err)
	}
	lister, ok := p.(provider.VolumeRecoverer)
	if !ok {
		return nil
	}
	volumes, err := lister.ListManagedVolumes(ctx)
	if err != nil {
		return fmt.Errorf("listing managed volumes from cloud: %w", err)
	}

	vs.mu.Lock()
	defer vs.mu.Unlock()
	params = sanitizePersistableParams(params)
	for _, volume := range volumes {
		if validateVolumeStoreKey(volume.VolumeID) != nil || volume.SizeBytes <= 0 ||
			strings.TrimSpace(volume.Provider) == "" || strings.TrimSpace(volume.Path) == "" {
			continue
		}
		path, _ := vs.path(volume.VolumeID)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			continue
		}
		record := &volumeRecord{
			VolumeID: volume.VolumeID, Provider: volume.Provider, Path: volume.Path,
			CapacityBytes: volume.SizeBytes, Params: maps.Clone(params),
		}
		if err := writeJSONAtomic(path, record); err != nil {
			return fmt.Errorf("persist recovered volume %s: %w", volume.VolumeID, err)
		}
	}
	return vs.writeManifestLocked()
}

func (vs *volumeStore) AnyParams() map[string]string {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	entries, err := os.ReadDir(vs.dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" || entry.Name() == manifestFileName {
			continue
		}
		volumeID := strings.TrimSuffix(entry.Name(), ".json")
		record, err := loadVolumeRecord(filepath.Join(vs.dir, entry.Name()), volumeID)
		if err == nil && record.Params["cloudProvider"] != "" {
			return maps.Clone(record.Params)
		}
	}
	return nil
}

func (vs *volumeStore) listAllLocked() ([]*volumeRecord, error) {
	entries, err := os.ReadDir(vs.dir)
	if err != nil {
		return nil, err
	}
	records := make([]*volumeRecord, 0, len(entries))
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" || entry.Name() == manifestFileName {
			continue
		}
		volumeID := strings.TrimSuffix(entry.Name(), ".json")
		record, err := loadVolumeRecord(filepath.Join(vs.dir, entry.Name()), volumeID)
		if err == nil {
			records = append(records, record)
		}
	}
	return records, nil
}

func (vs *volumeStore) writeManifestLocked() error {
	records, err := vs.listAllLocked()
	if err != nil {
		return err
	}
	manifest := volumeManifest{}
	for _, record := range records {
		manifest.VolumeIDs = append(manifest.VolumeIDs, record.VolumeID)
		if manifest.Params == nil && record.Params["cloudProvider"] != "" {
			manifest.Params = sanitizePersistableParams(record.Params)
		}
	}
	if manifest.Params == nil {
		if existing, err := vs.readManifestLocked(); err == nil {
			manifest.Params = sanitizePersistableParams(existing.Params)
		}
	}
	if len(manifest.Params) == 0 {
		if err := os.Remove(vs.manifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDirectory(vs.dir)
	}
	return writeJSONAtomic(vs.manifestPath(), &manifest)
}

func (vs *volumeStore) readManifestLocked() (*volumeManifest, error) {
	file, err := os.Open(vs.manifestPath())
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var manifest volumeManifest
	if err := json.NewDecoder(file).Decode(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (vs *volumeStore) loadManifestParams() map[string]string {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	manifest, err := vs.readManifestLocked()
	if err != nil || manifest.Params["cloudProvider"] == "" {
		return nil
	}
	return maps.Clone(manifest.Params)
}

func loadBootstrapParamsFile() map[string]string {
	path := strings.TrimSpace(os.Getenv(bootstrapParamsEnv))
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		vsLogger.Printf("failed to read bootstrap params: %v", err)
		return nil
	}
	defer file.Close()
	var params map[string]string
	if err := json.NewDecoder(file).Decode(&params); err != nil || params["cloudProvider"] == "" {
		vsLogger.Printf("invalid bootstrap params file")
		return nil
	}
	return sanitizePersistableParams(params)
}

func paramsFromEnv() map[string]string {
	cloudProvider := firstNonEmpty(os.Getenv("CSI_CLOUD_PROVIDER"), os.Getenv("CLOUD_PROVIDER"))
	if cloudProvider == "" {
		return nil
	}
	params := map[string]string{"cloudProvider": cloudProvider}
	switch cloudProvider {
	case "aws":
		setIfNonEmpty(params, "awsRegion", firstNonEmpty(os.Getenv("CSI_AWS_REGION"), os.Getenv("AWS_REGION")))
		setIfNonEmpty(params, "awsAvailabilityZone", firstNonEmpty(os.Getenv("CSI_AWS_AVAILABILITY_ZONE"), os.Getenv("AWS_AVAILABILITY_ZONE")))
		setIfNonEmpty(params, "awsVolumeType", os.Getenv("CSI_AWS_VOLUME_TYPE"))
		setIfNonEmpty(params, "awsKmsKeyId", os.Getenv("CSI_AWS_KMS_KEY_ID"))
	case "azure":
		setIfNonEmpty(params, "azureSubscriptionId", firstNonEmpty(os.Getenv("CSI_AZURE_SUBSCRIPTION_ID"), os.Getenv("AZURE_SUBSCRIPTION_ID")))
		setIfNonEmpty(params, "azureResourceGroup", firstNonEmpty(os.Getenv("CSI_AZURE_RESOURCE_GROUP"), os.Getenv("AZURE_RESOURCE_GROUP")))
		setIfNonEmpty(params, "azureLocation", firstNonEmpty(os.Getenv("CSI_AZURE_LOCATION"), os.Getenv("AZURE_LOCATION")))
		setIfNonEmpty(params, "azureDiskSKU", os.Getenv("CSI_AZURE_DISK_SKU"))
		setIfNonEmpty(params, "azureDiskEncryptionSetId", os.Getenv("CSI_AZURE_DISK_ENCRYPTION_SET_ID"))
	case "libvirt":
		setIfNonEmpty(params, "cloudProviderVolumePath", firstNonEmpty(os.Getenv("CSI_LIBVIRT_VOLUME_PATH"), os.Getenv("CLOUD_PROVIDER_VOLUME_PATH")))

	}
	return params
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func setIfNonEmpty(params map[string]string, key, value string) {
	if value != "" {
		params[key] = value
	}
}

func sanitizePersistableParams(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}
	result := maps.Clone(params)
	for key := range result {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""))
		if strings.Contains(normalized, "accesskey") || strings.Contains(normalized, "secret") ||
			strings.Contains(normalized, "securitytoken") || strings.Contains(normalized, "password") ||
			strings.Contains(normalized, "credential") {
			delete(result, key)
		}
	}
	return result
}
