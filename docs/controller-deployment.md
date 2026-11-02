# Separate controller and node deployment

The chart and raw examples run one controller Deployment and a node DaemonSet.
`--component=controller` serves controller and identity RPCs; `--component=node`
serves node and identity RPCs. The default `--component=all` remains available
for development and existing manually managed deployments.

The controller holds cloud credentials, runs provisioner/resizer sidecars, and
stores its journal on a ReadWriteOnce PVC. Set
`controller.state.storageClassName` to an existing **independent host-cluster
StorageClass**. Do not use this driver to provision its own journal. The
Deployment uses one replica and Recreate; provisioner leader election does not
make the journal a multi-writer database. Do not increase replicas or share the
journal with another controller process.

The node service has no cloud credentials and no controller journal mount. It
uses `cloud-volume-path` in the controller's VolumeContext to recover a stage or
publish after restart. A projected Kubernetes token is mounted only in the node
driver to read node topology labels; its ServiceAccount has only `get nodes`.
The registrar has no API token. Preserve region/zone node labels for AWS
WaitForFirstConsumer scheduling. For local libvirt tests without zone labels,
set `CSI_TOPOLOGY_ZONE` explicitly on the node driver.

Libvirt pools remain host-local. Set `controller.nodeSelector` with a concrete
`kubernetes.io/hostname`; the chart pins the libvirt node plugin to the same
host. Moving only the controller journal cannot migrate the libvirt disk files.

## Raw manifests

`hack/render-raw-manifests.sh` regenerates examples from the Helm templates.
Replace uppercase placeholders, including `HOST_STORAGE_CLASS`, provider
region/zone and `NODE_NAME`, before applying. Apply common namespace,
serviceaccount, RBAC and CSIDriver files, then exactly one provider's bootstrap
ConfigMap, controller, node DaemonSet and StorageClass. The libvirt node example
is named `daemonset.yaml`; other examples use `daemonset-<provider>.yaml`.
Credentials and cloud workload-identity trust remain operator configuration.
Do not apply the whole `deploy/` directory: it contains mutually exclusive
provider examples.

## Migration and uninstall

1. Stop provisioning and drain pods using these volumes. Back up all existing
   controller records, keeping their names and contents intact.
2. Stop the old combined DaemonSet. Collect records from every old node-local
   journal; reconcile duplicate names against cloud inventory before merging.
3. Provision the independent controller-state PVC and restore records into its
   `volumes/` directory. Keep ownership and permissions readable by the driver.
4. Apply ServiceAccounts/RBAC, bootstrap parameters and the controller, then the
   node DaemonSet. The new DaemonSet selector adds a component label; delete and
   recreate the old DaemonSet instead of trying to patch its immutable selector.
5. Verify Create/Delete and an existing-volume mount before resuming provisioning.

Helm uninstall retains the journal PVC (`helm.sh/resource-policy: keep`). Delete
workloads and data PVCs first, verify cloud disks have been released, uninstall,
and only then explicitly delete the state PVC if no recovery is needed. Back up
this PVC before upgrades. Pending operations require their original journal;
cloud discovery is not a replacement for the journal or its operation tokens.

## Metadata compatibility

The default `nodePlugin.metadataVersion=legacy` preserves the released
`volume-type`, `device`, `fstype`, `metadata` and `options` wire format. Publication
is atomic and incompatible same-target replays fail. Legacy CAA implementations
may ignore metadata mount options; retained OCI container `ro` options can still
restrict a container bind mount. That does not promise a read-only underlying
guest filesystem or block device.

`nodePlugin.metadataVersion=v1` adds explicit read-only intent, volume handle,
Pod UID, mount options and versioned validation. Enable it only with a CAA proxy
and guest that consume this contract. VOLUME_MOUNT_GROUP is not advertised by this change. Delegated mount-group
requests remain disabled until a separately enabled guest implementation can
apply the GID. Unsupported mount-group requests fail instead of being silently
accepted.

Drain and unpublish volumes before switching formats in either direction.
Startup cleanup accepts both formats but does not rewrite live metadata. A live
legacy/v1 mismatch fails publication without overwriting the existing file.
Host metadata is not attested authorization; confidential workloads still need
their independent measured guest policy.
