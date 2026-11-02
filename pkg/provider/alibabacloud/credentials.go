// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package alibabacloud

import (
	"context"

	"github.com/aliyun/credentials-go/credentials"
)

// The credential SDK has no context API and its default provider chain is
// mutable. Serialize resolution and keep at most one outstanding resolver per
// client. Cancellation stops the CSI caller immediately; an in-flight read-only
// credential refresh finishes under the SDK's own transport timeouts. It cannot
// subsequently issue an ECS request for a canceled operation.
type credentialResolver struct {
	source credentials.Credential
	busy   chan struct{}
}

func newCredentialResolver(source credentials.Credential) *credentialResolver {
	return &credentialResolver{source: source, busy: make(chan struct{}, 1)}
}

func (r *credentialResolver) resolve(ctx context.Context) (*credentials.CredentialModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case r.busy <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	type result struct {
		value *credentials.CredentialModel
		err   error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-r.busy }()
		value, err := r.source.GetCredential()
		done <- result{value, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resolved := <-done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return resolved.value, resolved.err
	}
}
