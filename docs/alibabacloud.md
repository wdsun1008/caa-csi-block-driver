# Alibaba Cloud block volumes

The provider creates, describes and deletes ESSD disks for CAA peer pods. The
CAA cloud provider and guest must separately support attaching and mounting
Alibaba disks. CSI does not attach disks to worker nodes or PodVMs.

Supported operations: filesystem-backed CreateVolume, idempotent retry,
lookup/reconciliation and DeleteVolume. Expansion, snapshots and clones are not
implemented for Alibaba Cloud; its chart StorageClass defaults to
`allowVolumeExpansion: false` and rejects explicit enablement. Existing providers
retain their expansion capabilities and the controller resizer sidecar.

## Installation

First configure the [controller state and migration requirements](controller-deployment.md).
The disk region and zone must match the peer-pod VM's region and zone. This is
an explicit single-zone deployment; worker-node topology is not a substitute for
CAA's PodVM placement configuration.

```sh
helm upgrade --install caa-csi charts/caa-csi-block-driver \
  --set provider=alibabacloud \
  --set controller.state.storageClassName=YOUR_HOST_STORAGE_CLASS \
  --set alibabacloud.region=YOUR_REGION \
  --set alibabacloud.zoneId=YOUR_ZONE \
  --set alibabacloud.clusterId=YOUR_UNIQUE_CLUSTER_ID \
  --set alibabacloud.credentialsSecret=caa-csi-alibaba-credentials
```

The Secret in the CSI namespace contains `ALIBABA_CLOUD_ACCESS_KEY_ID`,
`ALIBABA_CLOUD_ACCESS_KEY_SECRET` and, for temporary credentials, optional
`ALIBABA_CLOUD_SECURITY_TOKEN`. Only the controller mounts these credentials.
Do not place credentials in StorageClass parameters, metadata, values files
committed to Git, or controller operation records.

For ACK RRSA, leave `alibabacloud.credentialsSecret` empty and set:

```yaml
alibabacloud:
  rrsa:
    enabled: true
    roleArn: acs:ram::ACCOUNT_ID:role/ROLE_NAME
    oidcProviderArn: acs:ram::ACCOUNT_ID:oidc-provider/PROVIDER_NAME
```

Enable RRSA on the cluster and configure the RAM role's OIDC trust for that
provider, audience `sts.aliyuncs.com`, and the exact controller ServiceAccount
subject `system:serviceaccount:NAMESPACE:SERVICE_ACCOUNT`. The chart projects a
one-hour token at `/var/run/secrets/ack.alibabacloud.com/rrsa-tokens/token` and
sets the three Alibaba OIDC environment variables. Do not grant the node
ServiceAccount this role. Use least-privilege RAM permissions for
`ecs:CreateDisk`, `ecs:DescribeDisks` and `ecs:DeleteDisk`, plus the tag permission
required by your CreateDisk policy. Scope supported resource/tag conditions to
the configured region and cluster. PodVM attach/detach permissions belong to
CAA's identity, not this controller's identity.

When neither mechanism is configured the Alibaba SDK credential chain is used.
ECS API calls use caller cancellation and a 30-second HTTP limit with automatic
SDK retries disabled; retry waits also obey caller cancellation. Credential
resolution is serialized per client. Cancellation releases the CSI caller; an
in-flight read-only credential refresh finishes under the credential SDK's own
timeouts and cannot send an ECS request for the canceled operation.

## Parameters and capacity

`region`, `zoneId` and `clusterId` are mandatory. Optional parameters are
`diskCategory` (default `cloud_essd`), `performanceLevel` (default `PL0` for ESSD)
and `resourceGroupId`. Supported categories are `cloud_essd`, `cloud_auto` and
`cloud_essd_entry`; account and zone availability still apply. Capacity is rounded
up to GiB and to the selected ESSD performance tier's minimum. The normalized
capacity must fit the CSI request's LimitBytes. Requests above 65536 GiB or
incompatible parameters fail before CreateDisk.

## Recovery and ownership

The controller persists a random operation ID and normalized request fingerprint
before CreateDisk. Retries reuse ClientToken and reconcile tagged disks. A timeout
or cancellation is an unknown create outcome, so the original operation remains
pending. Explicit terminal rejections may retire the operation. A same-name request
with different capacity or capabilities is rejected instead of adopting a disk
with different semantics.

Cluster, driver, volume and operation/specification tags constrain reconciliation
and deletion. The logical CSI handle is distinct from the physical `d-*` ID.
Attached disks are not deleted. ECS may briefly report Available while deletion
still returns IncorrectDiskStatus.Initializing; this explicit transient error is
retried within the caller deadline, rechecking ownership and attachment of the
same physical disk before each retry. Only a typed disk-not-found result counts as
absence; generic HTTP 404 or authorization errors do not count as successful
cleanup. Retain the journal and cluster ID across restarts. Losing every journal
copy can lose pending-operation identities; inspect cloud inventory before any
manual recovery. Never clear ownership tags to force deletion.

Local fake-client and HTTP tests do not establish cloud permissions or guest mount
behavior. Run create/retry/restart/delete and an actual PodVM filesystem test in
your region before production use.
