// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/aliyun/credentials-go/credentials"

	"github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

const (
	volumeTagKey = "caa-csi-volume-id"
	gib          = int64(1024 * 1024 * 1024)
	waitTimeout  = 2 * time.Minute
)

type Config struct {
	Region string
	Zone   string
}

type AlibabaCloudProvider struct {
	config Config
	sdk    openapi.Config
}

func init() {
	provider.RegisterProvider("alibabacloud", func(params map[string]string) (provider.BlockVolumeProvider, error) {
		return NewAlibabaCloudProvider(params)
	})
}

func NewAlibabaCloudProvider(params map[string]string) (*AlibabaCloudProvider, error) {
	if params["alibabaRegion"] == "" {
		return nil, errors.New("alibabaRegion is required")
	}
	credential, err := credentials.NewCredential(nil)
	if err != nil {
		return nil, err
	}
	return &AlibabaCloudProvider{
		config: Config{Region: params["alibabaRegion"], Zone: params["alibabaZoneId"]},
		sdk: openapi.Config{
			RegionId: tea.String(params["alibabaRegion"]), Credential: credential,
			ReadTimeout: tea.Int(30000), ConnectTimeout: tea.Int(10000),
		},
	}, nil
}

// Generated ECS methods have no context parameter. Bind the HTTP request to
// the CSI context without changing the shared SDK configuration.
type contextHTTPClient struct{ ctx context.Context }

func (c contextHTTPClient) Call(req *http.Request, _ *http.Transport) (*http.Response, error) {
	client := http.Client{Timeout: 30 * time.Second}
	return client.Do(req.Clone(c.ctx))
}

func (p *AlibabaCloudProvider) client(ctx context.Context) (*ecs.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config := p.sdk
	config.HttpClient = contextHTTPClient{ctx}
	return ecs.NewClient(&config)
}

func (p *AlibabaCloudProvider) CreateVolume(ctx context.Context, volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	disk, err := p.findDisk(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	// NodeStageVolume also calls CreateVolume, with a default size when capacity
	// is absent from VolumeContext. Reuse the existing disk as other providers do.
	if disk != nil {
		return volumeInfo(disk), nil
	}
	if p.config.Zone == "" {
		return nil, errors.New("alibabaZoneId is required to create volumes")
	}
	size, err := bytesToGiB(sizeBytes)
	if err != nil {
		return nil, err
	}
	client, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.CreateDiskWithOptions(&ecs.CreateDiskRequest{
		RegionId: tea.String(p.config.Region), ZoneId: tea.String(p.config.Zone),
		DiskCategory: tea.String("cloud_essd"), PerformanceLevel: tea.String("PL0"), Size: tea.Int32(size),
		Tag: []*ecs.CreateDiskRequestTag{{Key: tea.String(volumeTagKey), Value: tea.String(volumeID)}},
	}, &dara.RuntimeOptions{Autoretry: tea.Bool(false)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("CreateDisk: %w", err)
	}
	if response == nil || response.Body == nil || tea.StringValue(response.Body.DiskId) == "" {
		return nil, errors.New("CreateDisk returned no disk ID")
	}
	ids, _ := json.Marshal([]string{tea.StringValue(response.Body.DiskId)})
	for {
		disks, err := p.describeDisks(ctx, &ecs.DescribeDisksRequest{DiskIds: tea.String(string(ids))})
		if err != nil {
			return nil, err
		}
		if len(disks) == 1 && (tea.StringValue(disks[0].Status) == "Available" || tea.StringValue(disks[0].Status) == "In_use") {
			return volumeInfo(disks[0]), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *AlibabaCloudProvider) DeleteVolume(ctx context.Context, volumeID string) error {
	disk, err := p.findDisk(ctx, volumeID)
	if err != nil || disk == nil {
		return err
	}
	if tea.StringValue(disk.InstanceId) != "" || tea.StringValue(disk.Status) == "In_use" {
		return fmt.Errorf("disk %s is attached", tea.StringValue(disk.DiskId))
	}
	client, err := p.client(ctx)
	if err != nil {
		return err
	}
	_, err = client.DeleteDiskWithOptions(&ecs.DeleteDiskRequest{DiskId: disk.DiskId}, &dara.RuntimeOptions{Autoretry: tea.Bool(false)})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var sdkError *tea.SDKError
	if errors.As(err, &sdkError) && tea.StringValue(sdkError.Code) == "InvalidDiskId.NotFound" {
		return nil
	}
	return err
}

func (p *AlibabaCloudProvider) GetVolumeInfo(ctx context.Context, volumeID string) (*provider.VolumeInfo, error) {
	disk, err := p.findDisk(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if disk == nil {
		return nil, fmt.Errorf("volume %s not found", volumeID)
	}
	return volumeInfo(disk), nil
}

func (p *AlibabaCloudProvider) VolumeExists(ctx context.Context, volumeID string) (bool, error) {
	disk, err := p.findDisk(ctx, volumeID)
	return disk != nil, err
}

func (p *AlibabaCloudProvider) findDisk(ctx context.Context, volumeID string) (*ecs.DescribeDisksResponseBodyDisksDisk, error) {
	if volumeID == "" {
		return nil, errors.New("volume ID is required")
	}
	disks, err := p.describeDisks(ctx, &ecs.DescribeDisksRequest{
		Tag: []*ecs.DescribeDisksRequestTag{{Key: tea.String(volumeTagKey), Value: tea.String(volumeID)}},
	})
	if err != nil {
		return nil, err
	}
	if len(disks) > 1 {
		return nil, fmt.Errorf("multiple Alibaba disks match volume %s", volumeID)
	}
	if len(disks) == 0 {
		return nil, nil
	}
	return disks[0], nil
}

// Use the existing cloud-recovery hook; no new controller journal is required.
func (p *AlibabaCloudProvider) ListManagedVolumes(ctx context.Context) ([]*provider.VolumeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	disks, err := p.describeDisks(ctx, &ecs.DescribeDisksRequest{
		Tag: []*ecs.DescribeDisksRequestTag{{Key: tea.String(volumeTagKey)}},
	})
	if err != nil {
		return nil, err
	}
	volumes := make([]*provider.VolumeInfo, 0, len(disks))
	for _, disk := range disks {
		if info := volumeInfo(disk); info.VolumeID != "" {
			volumes = append(volumes, info)
		}
	}
	return volumes, nil
}

func (p *AlibabaCloudProvider) describeDisks(ctx context.Context, req *ecs.DescribeDisksRequest) ([]*ecs.DescribeDisksResponseBodyDisksDisk, error) {
	client, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	req.RegionId, req.DiskType, req.PageSize = tea.String(p.config.Region), tea.String("data"), tea.Int32(100)
	var disks []*ecs.DescribeDisksResponseBodyDisksDisk
	for page := int32(1); ; page++ {
		req.PageNumber = tea.Int32(page)
		response, err := client.DescribeDisksWithOptions(req, &dara.RuntimeOptions{Autoretry: tea.Bool(false)})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, fmt.Errorf("DescribeDisks: %w", err)
		}
		if response == nil || response.Body == nil || response.Body.Disks == nil {
			return nil, errors.New("DescribeDisks returned no disk list")
		}
		disks = append(disks, response.Body.Disks.Disk...)
		if len(response.Body.Disks.Disk) == 0 || len(disks) >= int(tea.Int32Value(response.Body.TotalCount)) {
			return disks, nil
		}
	}
}

func volumeInfo(disk *ecs.DescribeDisksResponseBodyDisksDisk) *provider.VolumeInfo {
	info := &provider.VolumeInfo{
		Path: tea.StringValue(disk.DiskId), SizeBytes: int64(tea.Int32Value(disk.Size)) * gib, Provider: "alibabacloud",
		Metadata: map[string]string{"cloud-provider": "alibabacloud", "cloud-volume-path": tea.StringValue(disk.DiskId)},
	}
	if disk.Tags != nil {
		for _, tag := range disk.Tags.Tag {
			if tea.StringValue(tag.TagKey) == volumeTagKey {
				info.VolumeID = tea.StringValue(tag.TagValue)
			}
		}
	}
	return info
}

func bytesToGiB(size int64) (int32, error) {
	if size < 0 || size > 65536*gib {
		return 0, errors.New("ESSD PL0 size must be between 1 and 65536 GiB")
	}
	if size == 0 {
		return 1, nil
	}
	return int32((size + gib - 1) / gib), nil
}
