# Alibaba Cloud

This provider creates ESSD PL0 data disks in a configured region and zone. It
implements create, lookup, delete and the existing cloud-recovery interface.
Expansion, snapshots and cloning are not supported.

## CAA prerequisites

CSI provisions disks; CAA attaches them and the guest mounts them. Your CAA
build must support Alibaba data-disk attachment, retain disks when a PodVM is
deleted (`DeleteWithInstance=false`), and resolve Alibaba disk IDs in the guest.
The CAA release at `e3e0f004` and main at `1bab6e7f` lack the Alibaba attachment
path; their guest code also has no Alibaba-specific disk locator. Provisioning
a disk alone is not proof of a working PodVM mount.

This provider uses the existing `directvol` metadata and `cloud-volume-path`
field. It does not require a new metadata schema, delegated fsGroup, a split
controller deployment or a new controller journal. The CSI disk zone must
match the CAA PodVM zone; worker topology is not used to choose the zone.

## Credentials and installation

The Alibaba credential SDK's default chain is used. A Secret is the simplest
configuration for the existing combined controller/node DaemonSet. Supply
`ALIBABA_CLOUD_ACCESS_KEY_ID`, `ALIBABA_CLOUD_ACCESS_KEY_SECRET` and, for temporary
credentials, `ALIBABA_CLOUD_SECURITY_TOKEN` through a private env file:

```sh
kubectl create namespace caa-csi-block
kubectl -n caa-csi-block create secret generic caa-csi-alibaba-creds \
  --from-env-file=/path/to/private/alibaba.env
helm upgrade --install caa-csi charts/caa-csi-block-driver \
  --namespace caa-csi-block --set namespace.create=false \
  --set provider=alibabacloud \
  --set alibabacloud.region=cn-beijing --set alibabacloud.zoneId=cn-beijing-i \
  --set alibabacloud.credentialsSecret=caa-csi-alibaba-creds \
  --set storageClass.name=caa-alibaba-essd \
  --set image.repository=YOUR_REGISTRY/caa-csi-block-driver --set image.tag=YOUR_TAG
```

Use an image built from a revision containing this provider. The identity needs
`ecs:CreateDisk`, `ecs:DescribeDisks` and `ecs:DeleteDisk`. CAA needs its own
instance/disk attachment permissions. Credentials are needed by both controller
and node operations in the existing architecture; they are not StorageClass
parameters. Other default-chain credentials can be configured in the workload
environment; this change adds no RRSA-specific deployment mechanism.

For raw manifests, create the Secret as above, edit the region/zone in
`deploy/bootstrap-params-alibabacloud.yaml` and
`deploy/storageclass-alibabacloud.yaml`, and set your driver image in
`deploy/daemonset-alibabacloud.yaml`. Apply the existing `deploy/rbac.yaml` and
`deploy/csi-driver.yaml`, then those three Alibaba manifests.

## Scope

ESSD PL0 supports 1–65,536 GiB; byte requests are rounded up to GiB. See the
[CreateDisk API](https://www.alibabacloud.com/help/en/ecs/developer-reference/api-ecs-2014-05-26-createdisk).
Use GiB-sized PVC requests. Volume names are stored in `caa-csi-volume-id` tags
and must fit the 128-character tag value limit.

Like the existing providers, creation first looks up that tag and reuses an
existing disk. This does not add crash-safe operation logging or stronger
concurrent-create guarantees. Cloud errors are returned to the existing CSI
retry path, including a disk still initializing when deletion is requested.
RPC contexts bound ECS HTTP requests; credential refresh uses the credential
SDK's own timeouts. Keep the existing node-local volume state and bootstrap
configuration. Deleting a PVC with reclaim policy `Delete` deletes its disk.
