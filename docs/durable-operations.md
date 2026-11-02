# Durable create operations

Providers may implement `CapacityPlanner`, `OperationVolumeCreator` and
`TerminalCreateError` from `pkg/provider`. Existing providers keep their normal
create path. Capacity planning runs before the normalized request fingerprint is
saved, and the result must fit RequiredBytes/LimitBytes. The fingerprint includes
capabilities, content source and topology-resolved provider parameters.

For operation-aware providers the controller writes a pending record containing
a random operation ID before contacting the cloud. Retries and process restarts
reuse that ID. An ambiguous create error, timeout or cancellation preserves the
pending record. Only a provider's explicit terminal failure classification can
retire it. Once the provider returns a disk, a ready record is written atomically.
A retry with different same-name properties fails rather than reusing that disk.
Delete resumes a pending operation to identify and release any created disk.

Locks serialize same-volume requests within one process and obey cancellation
while waiting. This is not a multi-writer database: use the single-controller
[deployment](controller-deployment.md) with a backed-up persistent journal.
Legacy records are accepted without pretending they contained request fingerprints
or operation IDs that were never recorded. Losing the complete journal can lose
pending-operation identity, so cloud inventory alone cannot guarantee recovery.
