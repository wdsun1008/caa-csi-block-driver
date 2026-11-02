// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"context"
	"fmt"
	"net/http"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
)

// The generated ECS methods do not take context. Supply a request-local HTTP
// adapter instead of racing an uncancellable SDK call against a goroutine.
// Clients share the underlying HTTP connection pool, not mutable SDK state.
type sdkDiskClient struct {
	config     *openapi.Config
	http       *http.Client
	credential *credentialResolver
}

type contextHTTPClient struct {
	ctx    context.Context
	client *http.Client
}

func (c contextHTTPClient) Call(request *http.Request, _ *http.Transport) (*http.Response, error) {
	return c.client.Do(request.Clone(c.ctx))
}

func (c *sdkDiskClient) forRequest(ctx context.Context) (*ecs.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config := *c.config
	if c.credential != nil {
		resolved, err := c.credential.resolve(ctx)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			return nil, fmt.Errorf("Alibaba credential resolver returned no credentials")
		}
		config.Credential = nil
		config.AccessKeyId = resolved.AccessKeyId
		config.AccessKeySecret = resolved.AccessKeySecret
		config.SecurityToken = resolved.SecurityToken
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config.HttpClient = contextHTTPClient{ctx: ctx, client: c.http}
	return ecs.NewClient(&config)
}

func (c *sdkDiskClient) CreateDisk(ctx context.Context, request *ecs.CreateDiskRequest) (*ecs.CreateDiskResponse, error) {
	client, err := c.forRequest(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.CreateDiskWithOptions(request, &dara.RuntimeOptions{Autoretry: dara.Bool(false)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return response, err
}

func (c *sdkDiskClient) DeleteDisk(ctx context.Context, request *ecs.DeleteDiskRequest) (*ecs.DeleteDiskResponse, error) {
	client, err := c.forRequest(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.DeleteDiskWithOptions(request, &dara.RuntimeOptions{Autoretry: dara.Bool(false)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return response, err
}

func (c *sdkDiskClient) DescribeDisks(ctx context.Context, request *ecs.DescribeDisksRequest) (*ecs.DescribeDisksResponse, error) {
	client, err := c.forRequest(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.DescribeDisksWithOptions(request, &dara.RuntimeOptions{Autoretry: dara.Bool(false)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return response, err
}
