#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="$ROOT_DIR/bin/caa-csi-block-driver"
TEST_DIR=$(mktemp -d /tmp/csi-sanity-XXXXXX)
CSI_SANITY_VERSION=v5.5.0

cleanup() {
    if [ -n "${DRIVER_PID:-}" ] && kill -0 "$DRIVER_PID" 2>/dev/null; then
        kill "$DRIVER_PID" 2>/dev/null || true
        wait "$DRIVER_PID" 2>/dev/null || true
    fi
    rm -rf "$TEST_DIR"
}
trap cleanup EXIT

if [ ! -x "$BINARY" ]; then
    echo "Error: driver binary not found at $BINARY"
    echo "Run 'make build' first."
    exit 1
fi

echo "Installing csi-sanity ${CSI_SANITY_VERSION}..."
go install "github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@${CSI_SANITY_VERSION}"
CSI_SANITY="$(go env GOPATH)/bin/csi-sanity"

export KUBELET_ROOT_PATH="$TEST_DIR/kubelet"
export CSI_SANITY_TARGET_PATH=1
mkdir -p "$TEST_DIR"/{pool,volstore,kata-vols} "$KUBELET_ROOT_PATH/pods"

cat > "$TEST_DIR/params.yaml" <<EOF
cloudProvider: libvirt
cloudProviderVolumePath: $TEST_DIR/pool
EOF

cat > "$TEST_DIR/create-path.sh" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
: "${KUBELET_ROOT_PATH:?}"
pod_dir=$(mktemp -d "$KUBELET_ROOT_PATH/pods/csi-sanity-pod-XXXXXX")
volume_root="$pod_dir/volumes/kubernetes.io~csi"
mkdir -p "$volume_root"
mktemp -d "$volume_root/csi-sanity-volume-XXXXXX"
SCRIPT

cat > "$TEST_DIR/remove-path.sh" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
: "${KUBELET_ROOT_PATH:?}"
target="${1:?}"
case "$target" in
    "$KUBELET_ROOT_PATH"/pods/*/volumes/kubernetes.io~csi/*/target | \
        "$KUBELET_ROOT_PATH"/pods/*/volumes/kubernetes.io~csi/*)
        rm -rf -- "$target"
        ;;
    *)
        echo "refusing to remove non-canonical sanity path: $target" >&2
        exit 1
        ;;
esac
SCRIPT

cat > "$TEST_DIR/check-path.sh" <<'SCRIPT'
#!/usr/bin/env bash
if [ ! -e "$1" ]; then echo "not_found"
elif [ -f "$1" ]; then echo "file"
elif [ -d "$1" ]; then echo "directory"
else echo "other"; fi
SCRIPT

chmod +x "$TEST_DIR"/*.sh

KATA_DIRECT_VOLUME_ROOT_PATH="$TEST_DIR/kata-vols" \
CSI_VOLUME_STORE_DIR="$TEST_DIR/volstore" \
CSI_ALLOW_HOST_STATS_FALLBACK=1 \
CSI_TOPOLOGY_ZONE=csi-sanity \
"$BINARY" \
    --endpoint="unix://$TEST_DIR/csi.sock" \
    --drivername=caa-csi-block.csi.confidentialcontainers.io \
    --nodeid=test-node &
DRIVER_PID=$!
sleep 2

if ! kill -0 "$DRIVER_PID" 2>/dev/null; then
    echo "Error: driver failed to start"
    exit 1
fi

echo "Driver started (PID $DRIVER_PID), running csi-sanity..."
echo ""

SKIP="Snapshot|ListVolumes|GetCapacity|ControllerPublishVolume|ControllerUnpublishVolume"
SKIP+="|ExpandVolume|NodeExpandVolume|ModifyVolume"
SKIP+="|GroupController|volume from an existing source"

"$CSI_SANITY" \
    --csi.endpoint="unix://$TEST_DIR/csi.sock" \
    --csi.testvolumeparameters="$TEST_DIR/params.yaml" \
    --csi.createstagingpathcmd="$TEST_DIR/create-path.sh" \
    --csi.removestagingpathcmd="$TEST_DIR/remove-path.sh" \
    --csi.createmountpathcmd="$TEST_DIR/create-path.sh" \
    --csi.removemountpathcmd="$TEST_DIR/remove-path.sh" \
    --csi.checkpathcmd="$TEST_DIR/check-path.sh" \
    --ginkgo.skip="$SKIP" \
    "$@"
