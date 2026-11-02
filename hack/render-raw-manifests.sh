#!/usr/bin/env bash
# Regenerate the raw examples from the Helm chart. Replace the uppercase
# placeholders before applying. Apply exactly one provider's controller/node.
set -euo pipefail
cd "$(dirname "$0")/.."
common=(--set fullnameOverride=caa-csi-block-plugin --set serviceAccount.name=caa-csi-provisioner --set controller.state.storageClassName=HOST_STORAGE_CLASS)
for provider in aws azure libvirt; do
  args=("${common[@]}" --set "provider=$provider")
  case "$provider" in
    aws) args+=(--set aws.region=AWS_REGION --set aws.availabilityZone=AWS_ZONE --set aws.staticCredentials.enabled=true) ;;
    azure) args+=(--set azure.subscriptionId=AZURE_SUBSCRIPTION_ID --set azure.resourceGroup=AZURE_RESOURCE_GROUP --set azure.location=AZURE_LOCATION) ;;
    libvirt) args+=(--set 'controller.nodeSelector.kubernetes\.io/hostname=NODE_NAME') ;;
  esac
  for template in controller daemonset bootstrap-params storageclass; do
    output="deploy/$template-$provider.yaml"
    if [[ "$template" == daemonset && "$provider" == libvirt ]]; then output=deploy/daemonset.yaml; fi
    helm template caa-csi-block charts/caa-csi-block-driver "${args[@]}" --show-only "templates/$template.yaml" > "$output"
  done
  if [[ "$provider" == libvirt ]]; then
    for template in namespace csi-driver rbac serviceaccount; do
      helm template caa-csi-block charts/caa-csi-block-driver "${args[@]}" --show-only "templates/$template.yaml" > "deploy/$template.yaml"
    done
  fi
done
