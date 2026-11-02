// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/aliyun/credentials-go/credentials"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

const (
	providerName       = "alibabacloud"
	defaultCategory    = "cloud_essd"
	defaultPerformance = "PL0"
	volumeTagKey       = "caa-csi-volume-id"
	driverTagKey       = "caa-csi-driver"
	driverTagValue     = "caa-csi-block-driver"
	clusterIDTagKey    = "ClusterId"
	createOperationTag = "caa-csi-create-operation"
	createSpecTag      = "caa-csi-create-spec"
	durableVolumeTag   = "csi.alibabacloud.com/peerpod-durable"
	gib                = int64(1024 * 1024 * 1024)
	operationTimeout   = 3 * time.Minute
	pollInterval       = 2 * time.Second
)

var (
	logger             = log.New(log.Writer(), "[caa-csi/alibabacloud] ", log.LstdFlags|log.Lmsgprefix)
	diskIDPattern      = regexp.MustCompile(`^d-[A-Za-z0-9]+$`)
	operationIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
	invalidName        = regexp.MustCompile(`[^A-Za-z0-9_.:-]`)
)

func init() {
	provider.RegisterProvider(providerName, func(params map[string]string) (provider.BlockVolumeProvider, error) {
		return NewAlibabaCloudProvider(params)
	})
}

type Config struct {
	Region           string
	Zone             string
	Category         string
	PerformanceLevel string
	ResourceGroupID  string
	ClusterID        string
}

type diskClient interface {
	CreateDisk(context.Context, *ecs.CreateDiskRequest) (*ecs.CreateDiskResponse, error)
	DeleteDisk(context.Context, *ecs.DeleteDiskRequest) (*ecs.DeleteDiskResponse, error)
	DescribeDisks(context.Context, *ecs.DescribeDisksRequest) (*ecs.DescribeDisksResponse, error)
}

type AlibabaCloudProvider struct {
	client diskClient
	config Config
}

type terminalCreateError struct {
	err error
}

func (err *terminalCreateError) Error() string {
	return err.err.Error()
}

func (err *terminalCreateError) Unwrap() error {
	return err.err
}

// TerminalCreateFailure tells the controller that ECS explicitly rejected the
// request and the persisted operation may be retired before a later retry.
func (err *terminalCreateError) TerminalCreateFailure() bool {
	return true
}

var _ provider.BlockVolumeProvider = (*AlibabaCloudProvider)(nil)

func NewAlibabaCloudProvider(params map[string]string) (*AlibabaCloudProvider, error) {
	config, err := parseConfig(params)
	if err != nil {
		return nil, err
	}

	credential, err := credentials.NewCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Alibaba Cloud credential: %w", err)
	}
	client := &sdkDiskClient{config: &openapi.Config{

		RegionId: tea.String(config.Region),
	}, credential: newCredentialResolver(credential), http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	return newAlibabaCloudProviderWithClient(config, client), nil
}

func newAlibabaCloudProviderWithClient(config Config, client diskClient) *AlibabaCloudProvider {
	return &AlibabaCloudProvider{client: client, config: config}
}

func parseConfig(params map[string]string) (Config, error) {
	config := Config{
		Region:           firstValueOrEnv(params, "CAA_CSI_ALIBABA_REGION", "region", "alibabaRegion", "alibabaCloudRegion"),
		Zone:             firstValueOrEnv(params, "CAA_CSI_ALIBABA_ZONE_ID", "zoneId", "zone", "alibabaZoneId"),
		Category:         firstValueOrEnv(params, "CAA_CSI_ALIBABA_DISK_CATEGORY", "type", "diskCategory", "alibabaDiskCategory"),
		PerformanceLevel: firstValueOrEnv(params, "CAA_CSI_ALIBABA_PERFORMANCE_LEVEL", "performanceLevel", "alibabaPerformanceLevel"),
		ResourceGroupID:  firstValueOrEnv(params, "CAA_CSI_ALIBABA_RESOURCE_GROUP_ID", "resourceGroupId", "alibabaResourceGroupId"),
		ClusterID:        firstValueOrEnv(params, "CAA_CSI_ALIBABA_CLUSTER_ID", "clusterId", "alibabaClusterId"),
	}
	if config.Region == "" {
		return Config{}, errors.New("region is required for alibabacloud provider")
	}
	if config.Zone == "" {
		return Config{}, errors.New("zoneId is required for alibabacloud provider")
	}
	if config.ClusterID == "" {
		return Config{}, errors.New("clusterId is required for cluster-scoped Alibaba disk ownership")
	}
	if config.Category == "" {
		config.Category = defaultCategory
	}
	if config.Category != "cloud_essd" && config.Category != "cloud_auto" && config.Category != "cloud_essd_entry" {
		return Config{}, fmt.Errorf("unsupported Alibaba disk category %q", config.Category)
	}
	if config.PerformanceLevel == "" && config.Category == "cloud_essd" {
		config.PerformanceLevel = defaultPerformance
	}
	if config.PerformanceLevel != "" {
		switch config.PerformanceLevel {
		case "PL0", "PL1", "PL2", "PL3":
		default:
			return Config{}, fmt.Errorf("invalid Alibaba ESSD performance level %q", config.PerformanceLevel)
		}
	}
	return config, nil
}

func firstValue(params map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(params[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstValueOrEnv(params map[string]string, environment string, keys ...string) string {
	if value := firstValue(params, keys...); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(environment))
}

func (p *AlibabaCloudProvider) CreateVolume(ctx context.Context, volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	return nil, errors.New("Alibaba CreateVolume requires a controller-persisted operation ID")
}

// CreateVolumeWithOperation provisions or resumes one logical create. The
// controller persists operationID before this method can mutate cloud state.
func (p *AlibabaCloudProvider) CreateVolumeWithOperation(ctx context.Context, volumeID string, sizeBytes int64, operationID string) (*provider.VolumeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if volumeID == "" {
		return nil, errors.New("volume ID is required")
	}
	if !operationIDPattern.MatchString(operationID) {
		return nil, errors.New("a valid controller-persisted operation ID is required")
	}
	normalizedSize, err := p.NormalizeCapacity(sizeBytes)
	if err != nil {
		return nil, err
	}
	sizeGiB := normalizedSize / gib
	if existing, err := p.findDiskByLogicalID(ctx, volumeID); err != nil {
		return nil, err
	} else if existing != nil {
		return p.adoptExistingCreate(ctx, volumeID, sizeGiB, existing)
	}
	tags := []*ecs.CreateDiskRequestTag{
		{Key: tea.String(volumeTagKey), Value: tea.String(volumeID)},
		{Key: tea.String(driverTagKey), Value: tea.String(driverTagValue)},
		{Key: tea.String(durableVolumeTag), Value: tea.String("true")},
		{Key: tea.String(createSpecTag), Value: tea.String(p.createSpecFingerprint(volumeID, sizeGiB))},
		{Key: tea.String(createOperationTag), Value: tea.String(operationID)},
	}
	if p.config.ClusterID != "" {
		tags = append(tags, &ecs.CreateDiskRequestTag{Key: tea.String(clusterIDTagKey), Value: tea.String(p.config.ClusterID)})
	}
	request := &ecs.CreateDiskRequest{
		RegionId:         tea.String(p.config.Region),
		ZoneId:           tea.String(p.config.Zone),
		DiskName:         tea.String(diskName(volumeID)),
		DiskCategory:     tea.String(p.config.Category),
		PerformanceLevel: optionalString(p.config.PerformanceLevel),
		Size:             tea.Int32(int32(sizeGiB)),
		ClientToken:      tea.String("caa-csi-" + operationID),
		ResourceGroupId:  optionalString(p.config.ResourceGroupID),
		MultiAttach:      tea.String("Disabled"),
		Tag:              tags,
	}
	response, err := p.client.CreateDisk(ctx, request)
	if err != nil {
		if !isAmbiguousDiskMutationError(err) {
			return nil, &terminalCreateError{err: fmt.Errorf("CreateDisk for %s: %w", volumeID, err)}
		}
		// The service may have committed before the client observed a timeout.
		if reconciled, reconcileErr := p.reconcileCreateDisk(ctx, volumeID); reconcileErr == nil && reconciled != nil {
			logger.Printf("CreateDisk returned %v; reconciled logical volume %s to %s", err, volumeID, tea.StringValue(reconciled.DiskId))
			return p.adoptExistingCreate(ctx, volumeID, sizeGiB, reconciled)
		} else if reconcileErr != nil {
			return nil, fmt.Errorf("CreateDisk for %s returned %v and reconcile failed: %w", volumeID, err, reconcileErr)
		}
		return nil, fmt.Errorf("CreateDisk for %s: %w", volumeID, err)
	}
	if response == nil || response.Body == nil || !diskIDPattern.MatchString(tea.StringValue(response.Body.DiskId)) {
		return nil, errors.New("CreateDisk returned an invalid disk ID")
	}
	diskID := tea.StringValue(response.Body.DiskId)
	disk, err := p.waitForDiskReady(ctx, diskID)
	if err != nil {
		return nil, err
	}
	return p.adoptExistingCreate(ctx, volumeID, sizeGiB, disk)
}

func (p *AlibabaCloudProvider) adoptExistingCreate(ctx context.Context, volumeID string, sizeGiB int64, disk *ecs.DescribeDisksResponseBodyDisksDisk) (*provider.VolumeInfo, error) {
	status := tea.StringValue(disk.Status)
	if status != "Available" && status != "In_use" {
		var err error
		disk, err = p.waitForDiskReady(ctx, tea.StringValue(disk.DiskId))
		if err != nil {
			return nil, err
		}
	}
	if err := p.validateCreateSpec(volumeID, sizeGiB, disk); err != nil {
		return nil, err
	}
	return p.volumeInfo(volumeID, disk), nil
}

func (p *AlibabaCloudProvider) validateCreateSpec(volumeID string, sizeGiB int64, disk *ecs.DescribeDisksResponseBodyDisksDisk) error {
	diskID := tea.StringValue(disk.DiskId)
	if !diskIDPattern.MatchString(diskID) || tea.StringValue(disk.Type) != "data" ||
		tea.StringValue(disk.ZoneId) != p.config.Zone || tea.StringValue(disk.Category) != p.config.Category ||
		int64(tea.Int32Value(disk.Size)) != sizeGiB ||
		tagValue(disk, durableVolumeTag) != "true" ||
		tagValue(disk, createSpecTag) != p.createSpecFingerprint(volumeID, sizeGiB) {
		return fmt.Errorf("existing Alibaba disk %s does not match the immutable create specification", diskID)
	}
	if multiAttach := tea.StringValue(disk.MultiAttach); multiAttach != "" && multiAttach != "Disabled" {
		return fmt.Errorf("existing Alibaba disk %s unexpectedly enables multi-attach", diskID)
	}
	if p.config.PerformanceLevel != "" && tea.StringValue(disk.PerformanceLevel) != p.config.PerformanceLevel {
		return fmt.Errorf("existing Alibaba disk %s has performance level %q, expected %q", diskID, tea.StringValue(disk.PerformanceLevel), p.config.PerformanceLevel)
	}
	if p.config.ResourceGroupID != "" && tea.StringValue(disk.ResourceGroupId) != p.config.ResourceGroupID {
		return fmt.Errorf("existing Alibaba disk %s has a different resource group", diskID)
	}
	if tagValue(disk, createOperationTag) == "" {
		return fmt.Errorf("existing Alibaba disk %s has no create-operation identity", diskID)
	}
	return nil
}

func (p *AlibabaCloudProvider) DeleteVolume(ctx context.Context, volumeID string) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	disk, err := p.findDiskByLogicalID(ctx, volumeID)
	if err != nil {
		return err
	}
	return p.deleteDisk(ctx, disk)
}

func (p *AlibabaCloudProvider) deleteDisk(ctx context.Context, disk *ecs.DescribeDisksResponseBodyDisksDisk) error {
	if disk == nil {
		return nil
	}
	diskID := tea.StringValue(disk.DiskId)
	logicalID := tagValue(disk, volumeTagKey)
	for {
		if err := p.validateManagedDisk(disk, logicalID); err != nil {
			return err
		}
		if instanceID := tea.StringValue(disk.InstanceId); instanceID != "" {
			return fmt.Errorf("disk %s is attached to instance %s", diskID, instanceID)
		}
		switch tea.StringValue(disk.Status) {
		case "In_use", "Attaching", "Detaching":
			return fmt.Errorf("disk %s is attached or transitioning attachment", diskID)
		}
		_, err := p.client.DeleteDisk(ctx, &ecs.DeleteDiskRequest{DiskId: tea.String(diskID)})
		if err == nil || isNotFound(err) {
			return p.waitForDiskDeleted(ctx, diskID)
		}
		code, _, typed := diskSDKError(err)
		if !typed || code != "IncorrectDiskStatus.Initializing" {
			return fmt.Errorf("DeleteDisk %s: %w", diskID, err)
		}
		logger.Printf("DeleteDisk %s is still initializing; waiting before retry", diskID)
		// ECS can report Available while initialization still prevents deletion.
		// Retry only this explicit transient error, respecting caller cancellation,
		// and revalidate ownership/attachment of the same physical disk each time.
		if err := waitForPoll(ctx); err != nil {
			return err
		}
		disk, err = p.findDiskByID(ctx, diskID)
		if err != nil {
			return err
		}
		if disk == nil {
			return nil
		}
	}
}

func (p *AlibabaCloudProvider) GetVolumeInfo(ctx context.Context, volumeID string) (*provider.VolumeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	disk, err := p.findDiskByLogicalID(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if disk == nil {
		return nil, fmt.Errorf("volume %s not found", volumeID)
	}
	return p.volumeInfo(volumeID, disk), nil
}

func (p *AlibabaCloudProvider) VolumeExists(ctx context.Context, volumeID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	disk, err := p.findDiskByLogicalID(ctx, volumeID)
	return disk != nil, err
}

func (p *AlibabaCloudProvider) findDiskByLogicalID(ctx context.Context, volumeID string) (*ecs.DescribeDisksResponseBodyDisksDisk, error) {
	if strings.TrimSpace(volumeID) == "" {
		return nil, errors.New("logical volume ID is required")
	}
	request := &ecs.DescribeDisksRequest{
		RegionId: tea.String(p.config.Region), PageSize: tea.Int32(100),
		Tag: []*ecs.DescribeDisksRequestTag{
			{Key: tea.String(volumeTagKey), Value: tea.String(volumeID)},
			{Key: tea.String(driverTagKey), Value: tea.String(driverTagValue)},
			{Key: tea.String(durableVolumeTag), Value: tea.String("true")},
		},
	}
	if p.config.ClusterID != "" {
		request.Tag = append(request.Tag, &ecs.DescribeDisksRequestTag{Key: tea.String(clusterIDTagKey), Value: tea.String(p.config.ClusterID)})
	}
	disks, err := p.describe(ctx, request)
	if err != nil {
		return nil, err
	}
	if len(disks) == 0 {
		return nil, nil
	}
	if len(disks) != 1 {
		return nil, fmt.Errorf("logical volume %s matches %d Alibaba disks", volumeID, len(disks))
	}
	if err := p.validateManagedDisk(disks[0], volumeID); err != nil {
		return nil, err
	}
	return disks[0], nil
}

func (p *AlibabaCloudProvider) findDiskByID(ctx context.Context, diskID string) (*ecs.DescribeDisksResponseBodyDisksDisk, error) {
	if !diskIDPattern.MatchString(diskID) {
		return nil, fmt.Errorf("invalid Alibaba disk ID %q", diskID)
	}
	encoded, err := json.Marshal([]string{diskID})
	if err != nil {
		return nil, err
	}
	disks, err := p.describe(ctx, &ecs.DescribeDisksRequest{
		RegionId: tea.String(p.config.Region), PageSize: tea.Int32(100), DiskIds: tea.String(string(encoded)),
	})
	if err != nil {
		return nil, err
	}
	if len(disks) == 0 {
		return nil, nil
	}
	if len(disks) != 1 || tea.StringValue(disks[0].DiskId) != diskID {
		return nil, fmt.Errorf("disk ID %s matches an unexpected Alibaba disk set", diskID)
	}
	if err := p.validateManagedDisk(disks[0], ""); err != nil {
		return nil, err
	}
	return disks[0], nil
}

func (p *AlibabaCloudProvider) validateManagedDisk(disk *ecs.DescribeDisksResponseBodyDisksDisk, expectedLogicalID string) error {
	diskType := tea.StringValue(disk.Type)
	status := tea.StringValue(disk.Status)
	logicalID := tagValue(disk, volumeTagKey)
	typeMatches := diskType == "data" || (diskType == "" &&
		(status == "" || status == "Creating" || status == "Attaching" || status == "Detaching" || status == "ReIniting"))
	if tagValue(disk, driverTagKey) != driverTagValue ||
		tagValue(disk, durableVolumeTag) != "true" || logicalID == "" ||
		!typeMatches ||
		(p.config.ClusterID != "" && tagValue(disk, clusterIDTagKey) != p.config.ClusterID) {
		return fmt.Errorf("disk %s is not owned by this CAA CSI deployment", tea.StringValue(disk.DiskId))
	}
	if expectedLogicalID != "" && logicalID != expectedLogicalID {
		return fmt.Errorf("disk %s has logical volume identity %q, expected %q", tea.StringValue(disk.DiskId), logicalID, expectedLogicalID)
	}
	return nil
}

func (p *AlibabaCloudProvider) describe(ctx context.Context, request *ecs.DescribeDisksRequest) ([]*ecs.DescribeDisksResponseBodyDisksDisk, error) {
	response, err := p.client.DescribeDisks(ctx, request)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("DescribeDisks: %w", err)
	}
	if response == nil || response.Body == nil || response.Body.Disks == nil {
		return nil, errors.New("DescribeDisks returned an incomplete response")
	}
	return response.Body.Disks.Disk, nil
}

func (p *AlibabaCloudProvider) waitForDiskReady(ctx context.Context, diskID string) (*ecs.DescribeDisksResponseBodyDisksDisk, error) {
	deadline := time.Now().Add(operationTimeout)
	for {
		disk, err := p.findDiskByID(ctx, diskID)
		if err != nil {
			return nil, err
		}
		if disk != nil {
			status := tea.StringValue(disk.Status)
			switch status {
			case "Available", "In_use":
				return disk, nil
			case "", "Creating", "Attaching", "Detaching", "ReIniting":
				// Continue through documented transitional states. In_use is
				// ready as an attach may race the create observer.
			default:
				return nil, fmt.Errorf("disk %s entered non-ready status %q", diskID, status)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for disk %s to become ready", diskID)
		}
		if err := waitForPoll(ctx); err != nil {
			return nil, err
		}
	}
}

func (p *AlibabaCloudProvider) waitForDiskDeleted(ctx context.Context, diskID string) error {
	deadline := time.Now().Add(operationTimeout)
	for {
		disk, err := p.findDiskByID(ctx, diskID)
		if err != nil {
			return err
		}
		if disk == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for disk %s deletion", tea.StringValue(disk.DiskId))
		}
		if err := waitForPoll(ctx); err != nil {
			return err
		}
	}
}

func (p *AlibabaCloudProvider) volumeInfo(volumeID string, disk *ecs.DescribeDisksResponseBodyDisksDisk) *provider.VolumeInfo {
	diskID := tea.StringValue(disk.DiskId)
	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      diskID,
		SizeBytes: int64(tea.Int32Value(disk.Size)) * gib,
		Provider:  providerName,
		Metadata: map[string]string{
			"cloud-provider":    providerName,
			"cloud-volume-path": diskID,
		},
	}
}

func tagValue(disk *ecs.DescribeDisksResponseBodyDisksDisk, key string) string {
	if disk == nil || disk.Tags == nil {
		return ""
	}
	for _, tag := range disk.Tags.Tag {
		if tag != nil && tea.StringValue(tag.TagKey) == key {
			return tea.StringValue(tag.TagValue)
		}
	}
	return ""
}

func minimumSizeGiB(category, performance string) int64 {
	if category != "cloud_essd" {
		return 1
	}
	switch performance {
	case "PL1":
		return 20
	case "PL2":
		return 461
	case "PL3":
		return 1261
	default:
		return 1
	}
}

func (p *AlibabaCloudProvider) NormalizeCapacity(requiredBytes int64) (int64, error) {
	if requiredBytes < 0 {
		return 0, errors.New("capacity cannot be negative")
	}
	if requiredBytes == 0 {
		requiredBytes = gib
	}
	if requiredBytes > 65536*gib {
		return 0, fmt.Errorf("requested Alibaba disk size exceeds 65536 GiB")
	}
	sizeGiB := (requiredBytes + gib - 1) / gib
	if minimum := minimumSizeGiB(p.config.Category, p.config.PerformanceLevel); sizeGiB < minimum {
		sizeGiB = minimum
	}
	if sizeGiB > 65536 {
		return 0, fmt.Errorf("requested Alibaba disk size %d GiB exceeds 65536 GiB", sizeGiB)
	}
	return sizeGiB * gib, nil
}

func diskName(volumeID string) string {
	name := "csi-" + invalidName.ReplaceAllString(volumeID, "-")
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return tea.String(value)
}

func (p *AlibabaCloudProvider) createSpecFingerprint(volumeID string, sizeGiB int64) string {
	payload := strings.Join([]string{
		driverTagValue, p.config.ClusterID, volumeID, p.config.Region, p.config.Zone,
		p.config.Category, p.config.PerformanceLevel, strconv.FormatInt(sizeGiB, 10),
		p.config.ResourceGroupID,
	}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func (p *AlibabaCloudProvider) reconcileCreateDisk(ctx context.Context, volumeID string) (*ecs.DescribeDisksResponseBodyDisksDisk, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		disk, err := p.findDiskByLogicalID(ctx, volumeID)
		if err == nil && disk != nil {
			return disk, nil
		}
		if err != nil && !isAmbiguousDiskMutationError(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		if err := waitForPoll(ctx); err != nil {
			return nil, err
		}
	}
}

func isAmbiguousDiskMutationError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	if sdkCode, statusCode, typed := diskSDKError(err); typed {
		code := strings.ToLower(sdkCode)
		if statusCode == 408 || statusCode == 409 || statusCode == 429 ||
			strings.Contains(code, "timeout") || strings.Contains(code, "throttl") ||
			strings.Contains(code, "idempotentprocessing") {
			return true
		}
		return statusCode < 400 || statusCode >= 500
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"timeout", "timed out", "connection reset", "unexpected eof"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	// A malformed response or an unknown SDK failure does not prove that ECS
	// rejected the mutation. Keep the operation identity for a later retry.
	return true
}

// The generated v7 client uses dara internally, but its default error adapter
// converts service responses to tea.SDKError. Accept both typed representations;
// never infer disk absence from HTTP status or error-message substrings.
func diskSDKError(err error) (string, int, bool) {
	var teaError *tea.SDKError
	if errors.As(err, &teaError) {
		return tea.StringValue(teaError.Code), tea.IntValue(teaError.StatusCode), true
	}
	var daraError *dara.SDKError
	if errors.As(err, &daraError) {
		return dara.StringValue(daraError.Code), dara.IntValue(daraError.StatusCode), true
	}
	return "", 0, false
}

func isNotFound(err error) bool {
	code, _, typed := diskSDKError(err)
	return typed && code == "InvalidDiskId.NotFound"
}

func waitForPoll(ctx context.Context) error {
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
