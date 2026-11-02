// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"os/exec"
	"strings"
	"testing"
)

const chartPath = "../../charts/caa-csi-block-driver"

func renderChartTemplate(t *testing.T, template string, values ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	args := []string{"template", "runtime-test", chartPath, "--show-only", template}
	for _, value := range values {
		args = append(args, "--set", value)
	}
	output, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, output)
	}
	return string(output)
}

func genericChartValues() []string {
	return []string{
		"provider=aws",
		"aws.region=us-test-1",
		"aws.availabilityZone=us-test-1a",
		"aws.staticCredentials.enabled=true",
		"aws.staticCredentials.secretName=aws-controller-credentials",
		"controller.state.storageClassName=host-storage",
	}
}

func alibabaChartValues() []string {
	return []string{
		"provider=alibabacloud",
		"alibabacloud.region=cn-test",
		"alibabacloud.zoneId=cn-test-a",
		"alibabacloud.clusterId=cluster-test",
		"alibabacloud.credentialsSecret=aliyun-controller-credentials",
		"controller.state.storageClassName=host-storage",
	}
}

func TestHelmSeparatesControllerAndNodePrivileges(t *testing.T) {
	values := genericChartValues()
	controller := renderChartTemplate(t, "templates/controller.yaml", values...)
	for _, required := range []string{
		"kind: PersistentVolumeClaim", "helm.sh/resource-policy: keep",
		`storageClassName: "host-storage"`, "kind: Deployment", "type: Recreate",
		`"--component=controller"`, `"--leader-election=true"`,
		"name: AWS_ACCESS_KEY_ID", "name: aws-controller-credentials",
	} {
		if !strings.Contains(controller, required) {
			t.Fatalf("controller render does not contain %q:\n%s", required, controller)
		}
	}
	for _, forbidden := range []string{"--nodeid=", "kind: DaemonSet", "/var/lib/kubelet/pods"} {
		if strings.Contains(controller, forbidden) {
			t.Fatalf("controller render contains node-only value %q:\n%s", forbidden, controller)
		}
	}

	node := renderChartTemplate(t, "templates/daemonset.yaml", values...)
	for _, required := range []string{
		"kind: DaemonSet", "automountServiceAccountToken: false",
		`"--component=node"`, `"--nodeid=$(KUBE_NODE_NAME)"`,
	} {
		if !strings.Contains(node, required) {
			t.Fatalf("node render does not contain %q:\n%s", required, node)
		}
	}
	for _, forbidden := range []string{
		"csi-provisioner", "--component=controller", "AWS_ACCESS_KEY",
		"aws-controller-credentials", "controller-state", "bootstrap-params",
	} {
		if strings.Contains(node, forbidden) {
			t.Fatalf("node render contains controller-only value %q:\n%s", forbidden, node)
		}
	}
}

func TestHelmAlibabaStaticCredentialsStayOnController(t *testing.T) {
	values := alibabaChartValues()
	controller := renderChartTemplate(t, "templates/controller.yaml", values...)
	for _, required := range []string{
		"name: ALIBABA_CLOUD_ACCESS_KEY_ID", "name: ALIBABA_CLOUD_ACCESS_KEY_SECRET",
		"name: aliyun-controller-credentials",
	} {
		if !strings.Contains(controller, required) {
			t.Fatalf("Alibaba controller render does not contain %q:\n%s", required, controller)
		}
	}

	node := renderChartTemplate(t, "templates/daemonset.yaml", values...)
	for _, forbidden := range []string{"ALIBABA_CLOUD_ACCESS_KEY", "aliyun-controller-credentials"} {
		if strings.Contains(node, forbidden) {
			t.Fatalf("Alibaba node render contains controller credential %q:\n%s", forbidden, node)
		}
	}
}

func TestHelmAlibabaRRSAUsesProjectedShortLivedToken(t *testing.T) {
	values := alibabaChartValues()
	values[4] = "alibabacloud.credentialsSecret="
	values = append(values,
		"alibabacloud.rrsa.enabled=true",
		"alibabacloud.rrsa.roleArn=acs:ram::123:role/test",
		"alibabacloud.rrsa.oidcProviderArn=acs:ram::123:oidc-provider/test",
	)
	controller := renderChartTemplate(t, "templates/controller.yaml", values...)
	for _, required := range []string{
		"name: ALIBABA_CLOUD_ROLE_ARN", "name: ALIBABA_CLOUD_OIDC_PROVIDER_ARN",
		"name: ALIBABA_CLOUD_OIDC_TOKEN_FILE", "audience: sts.aliyuncs.com",
		"expirationSeconds: 3600",
	} {
		if !strings.Contains(controller, required) {
			t.Fatalf("RRSA render does not contain %q:\n%s", required, controller)
		}
	}
	if strings.Contains(controller, "ALIBABA_CLOUD_ACCESS_KEY_ID") {
		t.Fatalf("RRSA render contains static credentials:\n%s", controller)
	}
}

func TestHelmPreservesOptionalCDHParameters(t *testing.T) {
	values := genericChartValues()
	plain := renderChartTemplate(t, "templates/storageclass.yaml", values...)
	if strings.Contains(plain, "encrypt-type:") || strings.Contains(plain, "kbs-key-id:") {
		t.Fatalf("default StorageClass unexpectedly enables CDH:\n%s", plain)
	}

	values = append(values,
		"cdh.enabled=true",
		"cdh.encryptionType=luks2",
		"cdh.key=default/luks-key/workspace",
	)
	encrypted := renderChartTemplate(t, "templates/storageclass.yaml", values...)
	for _, required := range []string{
		`encrypt-type: "luks2"`,
		`kbs-key-id: "default/luks-key/workspace"`,
	} {
		if !strings.Contains(encrypted, required) {
			t.Fatalf("encrypted StorageClass does not contain %q:\n%s", required, encrypted)
		}
	}
}

func TestHelmPreservesTopologyAndResizePermissions(t *testing.T) {
	values := genericChartValues()
	controller := renderChartTemplate(t, "templates/controller.yaml", values...)
	for _, required := range []string{"name: csi-resizer", "--feature-gates=Topology=true"} {
		if !strings.Contains(controller, required) {
			t.Fatalf("missing controller feature %s", required)
		}
	}
	node := renderChartTemplate(t, "templates/daemonset.yaml", values...)
	for _, required := range []string{"name: node-api-access", "name: kube-root-ca.crt", "--direct-volume-metadata-version=legacy", "--guest-volume-mount-group=false"} {
		if !strings.Contains(node, required) {
			t.Fatalf("missing node configuration %s", required)
		}
	}
	rbac := renderChartTemplate(t, "templates/rbac.yaml", values...)
	if !strings.Contains(rbac, "persistentvolumeclaims/status") {
		t.Fatal("resizer cannot update PVC status")
	}
	sc := renderChartTemplate(t, "templates/storageclass.yaml", alibabaChartValues()...)
	if !strings.Contains(sc, "allowVolumeExpansion: false") {
		t.Fatal("Alibaba expansion must default to disabled")
	}
}

func TestHelmRejectsUnsupportedConfigurations(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	for _, bad := range [][]string{
		append(alibabaChartValues(), "storageClass.allowVolumeExpansion=true"),
		append(genericChartValues(), "nodePlugin.guestVolumeMountGroup=true"),
		append(genericChartValues(), "controller.state.storageClassName="),
		append(genericChartValues(), "nodePlugin.metadataVersion=unknown"),
	} {
		args := []string{"template", "invalid-test", chartPath}
		for _, value := range bad {
			args = append(args, "--set", value)
		}
		if output, err := exec.Command("helm", args...).CombinedOutput(); err == nil {
			t.Fatalf("invalid config %v accepted: %s", bad, output)
		}
	}
}
