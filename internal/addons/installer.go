/*
Copyright 2026 The Butler Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package addons

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	butlerv1alpha1 "github.com/butlerdotdev/butler-api/api/v1alpha1"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Installer handles addon installations on target clusters
type Installer struct {
	KubectlPath string
	HelmPath    string
	NodeIP      string
}

// ProviderCredentials holds credentials for infrastructure providers
type ProviderCredentials struct {
	Nutanix   *NutanixCredentials
	Harvester *HarvesterCredentials
	VSphere   *VSphereCredentials
	Proxmox   *ProxmoxCredentials
}

// NutanixCredentials holds Nutanix Prism Central credentials
type NutanixCredentials struct {
	Endpoint    string
	Username    string
	Password    string
	Port        string
	Insecure    bool
	ClusterUUID string
	SubnetUUID  string
	ImageUUID   string // optional
}

// HarvesterCredentials holds Harvester credentials
type HarvesterCredentials struct {
	Kubeconfig       []byte // optional - if connecting to external Harvester
	Namespace        string
	NetworkName      string
	ImageName        string // optional
	StorageClassName string // optional
}

// VSphereCredentials holds vSphere credentials
type VSphereCredentials struct {
	Server   string
	Username string
	Password string
}

// ProxmoxCredentials holds Proxmox credentials
type ProxmoxCredentials struct {
	Endpoint string
	Username string
	Password string
}

// NewInstaller creates a new addon installer
func NewInstaller(workDir string) *Installer {
	return &Installer{
		KubectlPath: "kubectl",
		HelmPath:    "helm",
	}
}

// SetNodeIP sets the node IP for direct access before VIP is available
func (i *Installer) SetNodeIP(ip string) {
	i.NodeIP = ip
}

func (i *Installer) writeKubeconfig(kubeconfig []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "kubeconfig-*")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(kubeconfig); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// makeKubeconfigInsecure modifies a kubeconfig to skip TLS verification
// This is needed because clusterctl doesn't properly handle self-signed CAs
func makeKubeconfigInsecure(kubeconfig []byte) ([]byte, error) {
	config, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// Set current context if empty
	if config.CurrentContext == "" && len(config.Contexts) > 0 {
		for name := range config.Contexts {
			config.CurrentContext = name
			break
		}
	}

	// Set insecure-skip-tls-verify for all clusters
	for _, cluster := range config.Clusters {
		cluster.InsecureSkipTLSVerify = true
		cluster.CertificateAuthorityData = nil
	}

	return clientcmd.Write(*config)
}

func (i *Installer) runHelm(ctx context.Context, kubeconfigPath string, args ...string) error {
	var fullArgs []string

	// repo add/update don't need kubeconfig or server flags
	if len(args) > 0 && args[0] == "repo" {
		fullArgs = args
		if len(args) > 1 && args[1] == "add" {
			fullArgs = append(fullArgs, "--insecure-skip-tls-verify")
		}
	} else {
		fullArgs = append(args, "--kubeconfig", kubeconfigPath, "--kube-insecure-skip-tls-verify")
		if i.NodeIP != "" {
			fullArgs = append(fullArgs, "--kube-apiserver", fmt.Sprintf("https://%s:6443", i.NodeIP))
		}
	}

	cmd := exec.CommandContext(ctx, i.HelmPath, fullArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm failed: %w, output: %s", err, string(output))
	}
	return nil
}

func (i *Installer) runKubectl(ctx context.Context, kubeconfigPath string, args ...string) error {
	fullArgs := append([]string{"--kubeconfig", kubeconfigPath, "--insecure-skip-tls-verify"}, args...)
	if i.NodeIP != "" {
		fullArgs = append(fullArgs, "--server", fmt.Sprintf("https://%s:6443", i.NodeIP))
	}

	cmd := exec.CommandContext(ctx, i.KubectlPath, fullArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl failed: %w, output: %s", err, string(output))
	}
	return nil
}

// ensurePrivilegedNamespace creates a namespace and labels it as privileged for PSA
func (i *Installer) ensurePrivilegedNamespace(ctx context.Context, kubeconfigPath string, namespace string) error {
	logger := log.FromContext(ctx)

	// Create namespace (ignore error if exists)
	i.runKubectl(ctx, kubeconfigPath, "create", "namespace", namespace)

	// Label as privileged - this is critical for running system components
	if err := i.runKubectl(ctx, kubeconfigPath, "label", "namespace", namespace,
		"pod-security.kubernetes.io/enforce=privileged",
		"pod-security.kubernetes.io/enforce-version=latest",
		"pod-security.kubernetes.io/warn=privileged",
		"pod-security.kubernetes.io/warn-version=latest",
		"pod-security.kubernetes.io/audit=privileged",
		"pod-security.kubernetes.io/audit-version=latest",
		"--overwrite"); err != nil {
		logger.Error(err, "Failed to label namespace as privileged", "namespace", namespace)
		return err
	}

	logger.Info("Namespace ready with privileged PSA", "namespace", namespace)
	return nil
}

// isSingleNodeCluster checks if the cluster has only one node.
// Used to determine if MetalLB needs special configuration for L2 announcements.
func (i *Installer) isSingleNodeCluster(ctx context.Context, kubeconfigPath string) bool {
	args := []string{
		"--kubeconfig", kubeconfigPath,
		"--insecure-skip-tls-verify",
		"get", "nodes",
		"-o", "jsonpath={.items[*].metadata.name}",
	}
	if i.NodeIP != "" {
		args = append(args, "--server", fmt.Sprintf("https://%s:6443", i.NodeIP))
	}

	cmd := exec.CommandContext(ctx, i.KubectlPath, args...)
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	nodes := strings.Fields(string(output))
	return len(nodes) == 1
}

// InstallKubeVip installs kube-vip for control plane HA ONLY.
func (i *Installer) InstallKubeVip(ctx context.Context, kubeconfig []byte, vip string, iface string, version string) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "v0.8.7"
	}
	if iface == "" {
		iface = "enp1s0"
	}

	// Extract CIDR from VIP if provided, otherwise default to /24
	vipCIDR := "24"
	if strings.Contains(vip, "/") {
		parts := strings.Split(vip, "/")
		vip = parts[0]
		vipCIDR = parts[1]
	}

	logger.Info("Installing kube-vip (control plane VIP only)", "vip", vip, "cidr", vipCIDR, "interface", iface, "version", version)

	// Ensure kube-system is privileged (should already be, but be safe)
	i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "kube-system")

	manifest := fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: kube-vip
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kube-vip
rules:
  - apiGroups: [""]
    resources: ["services", "services/status", "nodes", "endpoints"]
    verbs: ["list", "get", "watch", "update"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["list", "get", "watch", "create", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kube-vip
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kube-vip
subjects:
  - kind: ServiceAccount
    name: kube-vip
    namespace: kube-system
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: kube-vip
  namespace: kube-system
  labels:
    app.kubernetes.io/name: kube-vip
    app.kubernetes.io/component: control-plane-vip
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kube-vip
  template:
    metadata:
      labels:
        app.kubernetes.io/name: kube-vip
    spec:
      serviceAccountName: kube-vip
      hostNetwork: true
      tolerations:
        - effect: NoSchedule
          operator: Exists
        - effect: NoExecute
          operator: Exists
      nodeSelector:
        node-role.kubernetes.io/control-plane: ""
      containers:
        - name: kube-vip
          image: ghcr.io/kube-vip/kube-vip:%s
          args:
            - manager
          env:
            - name: vip_arp
              value: "true"
            - name: port
              value: "6443"
            - name: vip_interface
              value: "%s"
            - name: vip_address
              value: "%s"
            - name: vip_cidr
              value: "%s"
            - name: cp_enable
              value: "true"
            - name: cp_namespace
              value: kube-system
            # NOTE: svc_enable is intentionally NOT set (defaults to false)
            # MetalLB handles LoadBalancer services - do not enable here
            # to avoid IP conflicts between kube-vip and MetalLB
            - name: vip_leaderelection
              value: "true"
            - name: vip_leasename
              value: kube-vip-lease
            - name: vip_leaseduration
              value: "5"
            - name: vip_renewdeadline
              value: "3"
            - name: vip_retryperiod
              value: "1"
          securityContext:
            capabilities:
              add:
                - NET_ADMIN
                - NET_RAW
`, version, iface, vip, vipCIDR)

	tmpFile, err := os.CreateTemp("", "kube-vip-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(manifest); err != nil {
		return err
	}
	tmpFile.Close()

	return i.runKubectl(ctx, kubeconfigPath, "apply", "-f", tmpFile.Name())
}

// InstallCilium installs Cilium CNI
func (i *Installer) InstallCilium(ctx context.Context, kubeconfig []byte, version string, hubbleEnabled bool) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "1.17.0"
	}

	logger.Info("Installing Cilium", "version", version)

	// Ensure kube-system is privileged
	i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "kube-system")

	// Add Cilium repo
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "add", "cilium", "https://helm.cilium.io/"); err != nil {
		logger.Info("helm repo add failed (may already exist)", "error", err)
	}
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "update"); err != nil {
		logger.Info("helm repo update failed", "error", err)
	}

	args := []string{
		"upgrade", "--install", "cilium", "cilium/cilium",
		"--version", version,
		"--namespace", "kube-system",
		"--set", "ipam.mode=kubernetes",
		"--set", "kubeProxyReplacement=true",
		"--set", "securityContext.capabilities.ciliumAgent={CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}",
		"--set", "securityContext.capabilities.cleanCiliumState={NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}",
		"--set", "cgroup.autoMount.enabled=false",
		"--set", "cgroup.hostRoot=/sys/fs/cgroup",
		"--set", "k8sServiceHost=localhost",
		"--set", "k8sServicePort=7445",
	}

	if hubbleEnabled {
		args = append(args,
			"--set", "hubble.relay.enabled=true",
			"--set", "hubble.ui.enabled=true",
		)
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install Cilium: %w", err)
	}

	return nil
}

// InstallCertManager installs cert-manager for TLS certificate management
func (i *Installer) InstallCertManager(ctx context.Context, kubeconfig []byte, version string) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "v1.16.2"
	}

	logger.Info("Installing cert-manager", "version", version)

	// Ensure namespace exists and is privileged
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "cert-manager"); err != nil {
		return fmt.Errorf("failed to prepare cert-manager namespace: %w", err)
	}

	// Add Jetstack repo
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "add", "jetstack", "https://charts.jetstack.io"); err != nil {
		logger.Info("helm repo add failed (may already exist)", "error", err)
	}
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "update"); err != nil {
		logger.Info("helm repo update failed", "error", err)
	}

	args := []string{
		"upgrade", "--install", "cert-manager", "jetstack/cert-manager",
		"--namespace", "cert-manager",
		"--version", version,
		"--set", "crds.enabled=true",
		"--wait",
		"--timeout", "5m",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install cert-manager: %w", err)
	}

	return nil
}

// InstallLonghorn installs Longhorn distributed storage
func (i *Installer) InstallLonghorn(ctx context.Context, kubeconfig []byte, version string, replicaCount int32) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "1.7.2"
	}
	if replicaCount == 0 {
		replicaCount = 2
	}

	logger.Info("Installing Longhorn", "version", version)

	// Ensure namespace exists and is privileged BEFORE helm install
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "longhorn-system"); err != nil {
		return fmt.Errorf("failed to prepare longhorn-system namespace: %w", err)
	}

	// Add Longhorn repo
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "add", "longhorn", "https://charts.longhorn.io"); err != nil {
		logger.Info("helm repo add failed (may already exist)", "error", err)
	}
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "update"); err != nil {
		logger.Info("helm repo update failed", "error", err)
	}

	args := []string{
		"upgrade", "--install", "longhorn", "longhorn/longhorn",
		"--version", version,
		"--namespace", "longhorn-system",
		"--set", fmt.Sprintf("defaultSettings.defaultReplicaCount=%d", replicaCount),
		"--set", "defaultSettings.defaultDataPath=/var/lib/longhorn",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install Longhorn: %w", err)
	}

	return nil
}

// InstallMetalLB installs MetalLB load balancer with the specified address pool.
func (i *Installer) InstallMetalLB(ctx context.Context, kubeconfig []byte, addressPool string, topology string) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if addressPool == "" {
		return fmt.Errorf("addressPool is required for MetalLB installation")
	}

	logger.Info("Installing MetalLB", "addressPool", addressPool)

	// Ensure namespace exists and is privileged BEFORE helm install
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "metallb-system"); err != nil {
		return fmt.Errorf("failed to prepare metallb-system namespace: %w", err)
	}

	// For single-node clusters, remove the exclude-from-external-load-balancers label
	// from control plane nodes so MetalLB L2 can announce from them.
	// See: https://github.com/metallb/metallb/issues/2676
	if topology == "single-node" {
		logger.Info("Single-node topology, enabling MetalLB L2 on control plane node")
		if err := i.runKubectl(ctx, kubeconfigPath, "label", "nodes", "--all",
			"node.kubernetes.io/exclude-from-external-load-balancers-"); err != nil {
			logger.Info("Failed to remove exclude label (may not exist)", "error", err)
		}
	}

	// Add MetalLB repo
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "add", "metallb", "https://metallb.github.io/metallb"); err != nil {
		logger.Info("helm repo add failed (may already exist)", "error", err)
	}
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "update"); err != nil {
		logger.Info("helm repo update failed", "error", err)
	}

	args := []string{
		"upgrade", "--install", "metallb", "metallb/metallb",
		"--namespace", "metallb-system",
		"--wait",
		"--timeout", "5m",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install MetalLB: %w", err)
	}

	// Create IP address pool
	poolManifest := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: default-pool
  namespace: metallb-system
spec:
  addresses:
    - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: default
  namespace: metallb-system
spec:
  ipAddressPools:
    - default-pool
`, addressPool)

	tmpFile, err := os.CreateTemp("", "metallb-pool-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(poolManifest); err != nil {
		return err
	}
	tmpFile.Close()

	return i.runKubectl(ctx, kubeconfigPath, "apply", "-f", tmpFile.Name())
}

// InstallTraefik installs Traefik ingress controller
func (i *Installer) InstallTraefik(ctx context.Context, kubeconfig []byte, version string) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "34.3.0"
	}

	logger.Info("Installing Traefik", "version", version)

	// Ensure namespace exists and is privileged
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "traefik"); err != nil {
		return fmt.Errorf("failed to prepare traefik namespace: %w", err)
	}

	// Add Traefik repo
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "add", "traefik", "https://traefik.github.io/charts"); err != nil {
		logger.Info("helm repo add failed (may already exist)", "error", err)
	}
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "update"); err != nil {
		logger.Info("helm repo update failed", "error", err)
	}

	args := []string{
		"upgrade", "--install", "traefik", "traefik/traefik",
		"--namespace", "traefik",
		"--version", version,
		"--set", "ingressClass.enabled=true",
		"--set", "ingressClass.isDefaultClass=true",
		"--set", "service.type=LoadBalancer",
		"--wait",
		"--timeout", "5m",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install Traefik: %w", err)
	}

	return nil
}

// InstallGatewayAPI installs Gateway API CRDs (required before Steward for TLSRoute support)
func (i *Installer) InstallGatewayAPI(ctx context.Context, kubeconfig []byte, version string) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "v1.2.0"
	}

	logger.Info("Installing Gateway API CRDs", "version", version)

	// Install experimental channel (includes TLSRoute which Steward uses for SNI-based routing)
	url := fmt.Sprintf("https://github.com/kubernetes-sigs/gateway-api/releases/download/%s/experimental-install.yaml", version)

	if err := i.runKubectl(ctx, kubeconfigPath, "apply", "-f", url); err != nil {
		return fmt.Errorf("failed to install Gateway API CRDs: %w", err)
	}

	// Wait for CRDs to be established
	crds := []string{
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"tlsroutes.gateway.networking.k8s.io",
	}

	for _, crd := range crds {
		if err := i.runKubectl(ctx, kubeconfigPath, "wait", "--for=condition=Established",
			"crd", crd, "--timeout=60s"); err != nil {
			logger.Info("Gateway API CRD wait timeout (may already be ready)", "crd", crd)
		}
	}

	logger.Info("Gateway API CRDs installed successfully")
	return nil
}

// // InstallStewardCRDs installs Steward CRDs separately (optional - main chart includes CRDs)
// func (i *Installer) InstallStewardCRDs(ctx context.Context, kubeconfig []byte, version string) error {
// 	logger := log.FromContext(ctx)
// 	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
// 	if err != nil {
// 		return err
// 	}
// 	defer cleanup()
//
// 	if version == "" {
// 		version = "0.1.0"
// 	}
//
// 	logger.Info("Installing Steward CRDs", "version", version)
//
// 	// Install Steward CRDs via OCI registry
// 	args := []string{
// 		"upgrade", "--install", "steward-crds",
// 		"oci://ghcr.io/butlerdotdev/charts/steward-crds",
// 		"--namespace", "steward-system",
// 		"--create-namespace",
// 		"--version", version,
// 		"--wait",
// 		"--timeout", "2m",
// 	}
//
// 	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
// 		return fmt.Errorf("failed to install Steward CRDs: %w", err)
// 	}
//
// 	// Wait for CRDs to be established
// 	crds := []string{
// 		"tenantcontrolplanes.steward.butlerlabs.dev",
// 		"datastores.steward.butlerlabs.dev",
// 	}
//
// 	for _, crd := range crds {
// 		if err := i.runKubectl(ctx, kubeconfigPath, "wait", "--for=condition=Established",
// 			"crd", crd, "--timeout=60s"); err != nil {
// 			logger.Info("Steward CRD wait timeout (may already be ready)", "crd", crd)
// 		}
// 	}
//
// 	logger.Info("Steward CRDs installed successfully")
// 	return nil
// }

// InstallSteward installs Steward for hosted control planes (replaces Kamaji)
func (i *Installer) InstallSteward(ctx context.Context, kubeconfig []byte, version string) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "0.1.0"
	}

	logger.Info("Installing Steward", "version", version)

	// Ensure namespace exists and is privileged
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "steward-system"); err != nil {
		return fmt.Errorf("failed to prepare steward-system namespace: %w", err)
	}

	// Install Steward via OCI registry (no helm repo add needed for OCI)
	args := []string{
		"upgrade", "--install", "steward",
		"oci://ghcr.io/butlerdotdev/charts/steward",
		"--namespace", "steward-system",
		"--version", version,
		"--set", "image.repository=ghcr.io/butlerdotdev/steward",
		"--set", "image.tag=v0.1.0-alpha",
		"--set", "steward-etcd.deploy=true",
		"--wait",
		"--timeout", "5m",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install Steward: %w", err)
	}

	return nil
}

// InstallFlux installs FluxCD for GitOps
func (i *Installer) InstallFlux(ctx context.Context, kubeconfig []byte) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	logger.Info("Installing Flux")

	// Ensure flux-system namespace is privileged
	i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "flux-system")

	// Use flux CLI to install
	fullArgs := []string{"install", "--kubeconfig", kubeconfigPath}
	if i.NodeIP != "" {
		// Flux doesn't support --kube-apiserver, we need to modify kubeconfig
		// For now, skip if using node IP override
		logger.Info("Skipping Flux installation - VIP not yet available")
		return nil
	}

	cmd := exec.CommandContext(ctx, "flux", fullArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if flux CLI is available
		if strings.Contains(string(output), "not found") || strings.Contains(err.Error(), "executable file not found") {
			logger.Info("Flux CLI not available, skipping Flux installation")
			return nil
		}
		return fmt.Errorf("failed to install Flux: %w, output: %s", err, string(output))
	}

	return nil
}

// InstallButler installs Butler components on the target cluster
func (i *Installer) InstallButler(ctx context.Context, kubeconfig []byte) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	logger.Info("Installing Butler components")

	// Ensure butler-system namespace is privileged
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "butler-system"); err != nil {
		return fmt.Errorf("failed to prepare butler-system namespace: %w", err)
	}

	logger.Info("Butler namespace created - full installation pending")

	return nil
}

// InstallButlerCRDs installs Butler platform CRDs via Helm chart
func (i *Installer) InstallButlerCRDs(ctx context.Context, kubeconfig []byte, version string) error {
	logger := log.FromContext(ctx)
	logger.Info("Installing Butler platform CRDs", "version", version)

	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	defer cleanup()

	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "butler-system"); err != nil {
		return fmt.Errorf("failed to prepare butler-system namespace: %w", err)
	}

	if version == "" {
		version = "0.1.0"
	}

	args := []string{
		"upgrade", "--install",
		"butler-crds",
		"oci://ghcr.io/butlerdotdev/charts/butler-crds",
		"--version", version,
		"-n", "butler-system",
		"--wait",
		"--timeout", "5m",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install butler-crds: %w", err)
	}

	// Wait for all Butler CRDs to be established (discover dynamically)
	if err := i.waitForButlerCRDs(ctx, kubeconfigPath); err != nil {
		logger.Info("Some CRDs may not be ready yet", "error", err)
	}

	// Create default tenant namespace
	logger.Info("Creating butler-tenants namespace")
	i.runKubectl(ctx, kubeconfigPath, "create", "namespace", "butler-tenants")

	logger.Info("Butler platform CRDs installed successfully")
	return nil
}

func (i *Installer) InstallButlerAddons(ctx context.Context, kubeconfig []byte, version string) error {
	logger := log.FromContext(ctx)
	logger.Info("Installing Butler addon definitions", "version", version)

	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	defer cleanup()

	if version == "" {
		version = "0.1.0"
	}

	args := []string{
		"upgrade", "--install",
		"butler-addons",
		"oci://ghcr.io/butlerdotdev/charts/butler-addons",
		"--version", version,
		"-n", "butler-system",
		"--wait",
		"--timeout", "2m",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install butler-addons: %w", err)
	}

	logger.Info("Butler addon definitions installed successfully")
	return nil
}

// waitForButlerCRDs discovers and waits for all butler.butlerlabs.dev CRDs
func (i *Installer) waitForButlerCRDs(ctx context.Context, kubeconfigPath string) error {
	logger := log.FromContext(ctx)

	// Get all CRDs in butler.butlerlabs.dev group
	cmd := exec.CommandContext(ctx, i.KubectlPath,
		"--kubeconfig", kubeconfigPath,
		"--insecure-skip-tls-verify",
		"get", "crd",
		"-o", "jsonpath={.items[*].metadata.name}",
	)

	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list CRDs: %w", err)
	}

	// Filter to butler CRDs and wait for each
	allCRDs := strings.Fields(string(output))
	for _, crd := range allCRDs {
		if !strings.HasSuffix(crd, ".butler.butlerlabs.dev") {
			continue
		}

		logger.Info("Waiting for CRD", "crd", crd)
		args := []string{
			"wait", "--for=condition=Established",
			"crd", crd,
			"--timeout=60s",
		}
		if err := i.runKubectl(ctx, kubeconfigPath, args...); err != nil {
			logger.Info("CRD wait timeout (may already be ready)", "crd", crd)
		}
	}

	return nil
}

// InstallInitialProviderConfig creates the initial ProviderConfig CR and credentials secret
// on the management cluster based on the provider used during bootstrap.
func (i *Installer) InstallInitialProviderConfig(ctx context.Context, kubeconfig []byte, providerType string, creds *ProviderCredentials) error {
	logger := log.FromContext(ctx)
	logger.Info("Creating initial ProviderConfig", "provider", providerType)

	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	defer cleanup()

	// Ensure butler-system namespace exists
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "butler-system"); err != nil {
		return fmt.Errorf("failed to create butler-system namespace: %w", err)
	}

	var secretManifest, providerConfigManifest string

	switch providerType {
	case "nutanix":
		if creds == nil || creds.Nutanix == nil {
			return fmt.Errorf("nutanix credentials required")
		}
		secretManifest, providerConfigManifest = i.generateNutanixProviderConfig(creds.Nutanix)
	case "harvester":
		if creds == nil || creds.Harvester == nil {
			return fmt.Errorf("harvester credentials required")
		}
		secretManifest, providerConfigManifest = i.generateHarvesterProviderConfig(creds.Harvester)
	default:
		logger.Info("Provider type not yet supported for ProviderConfig creation, skipping", "provider", providerType)
		return nil
	}

	// Apply secret first (if any)
	if secretManifest != "" {
		tmpFile, err := os.CreateTemp("", "provider-secret-*.yaml")
		if err != nil {
			return err
		}
		defer os.Remove(tmpFile.Name())

		if _, err := tmpFile.WriteString(secretManifest); err != nil {
			return err
		}
		tmpFile.Close()

		if err := i.runKubectl(ctx, kubeconfigPath, "apply", "-f", tmpFile.Name()); err != nil {
			return fmt.Errorf("failed to apply provider credentials secret: %w", err)
		}
		logger.Info("Created provider credentials secret")
	}

	// Apply ProviderConfig
	tmpFile, err := os.CreateTemp("", "providerconfig-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(providerConfigManifest); err != nil {
		return err
	}
	tmpFile.Close()

	if err := i.runKubectl(ctx, kubeconfigPath, "apply", "-f", tmpFile.Name()); err != nil {
		return fmt.Errorf("failed to apply ProviderConfig: %w", err)
	}

	logger.Info("Initial ProviderConfig created successfully", "provider", providerType)
	return nil
}

func (i *Installer) generateNutanixProviderConfig(creds *NutanixCredentials) (string, string) {
	port := creds.Port
	if port == "" {
		port = "9440"
	}

	endpoint := creds.Endpoint
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		endpoint = fmt.Sprintf("https://%s:%s", creds.Endpoint, port)
	}

	secretManifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: nutanix-credentials
  namespace: butler-system
type: Opaque
stringData:
  username: "%s"
  password: "%s"
`, creds.Username, creds.Password)

	// Build nutanix config section
	nutanixConfig := fmt.Sprintf(`    endpoint: "%s"
    port: %s
    insecure: %t
    clusterUUID: "%s"
    subnetUUID: "%s"`, endpoint, port, creds.Insecure, creds.ClusterUUID, creds.SubnetUUID)

	// Add optional imageUUID if provided
	if creds.ImageUUID != "" {
		nutanixConfig += fmt.Sprintf(`
    imageUUID: "%s"`, creds.ImageUUID)
	}

	providerConfigManifest := fmt.Sprintf(`apiVersion: butler.butlerlabs.dev/v1alpha1
kind: ProviderConfig
metadata:
  name: nutanix
  namespace: butler-system
spec:
  provider: nutanix
  credentialsRef:
    name: nutanix-credentials
    namespace: butler-system
  nutanix:
%s
`, nutanixConfig)

	return secretManifest, providerConfigManifest
}

func (i *Installer) generateHarvesterProviderConfig(creds *HarvesterCredentials) (string, string) {
	namespace := creds.Namespace
	if namespace == "" {
		namespace = "default"
	}

	var secretManifest string

	// Always create the secret for consistency
	if len(creds.Kubeconfig) > 0 {
		// External kubeconfig provided - base64 encode for data field
		secretManifest = fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: harvester-kubeconfig
  namespace: butler-system
type: Opaque
data:
  kubeconfig: %s
`, base64.StdEncoding.EncodeToString(creds.Kubeconfig))
	} else {
		// No external kubeconfig - create placeholder for in-cluster usage
		// The provider will detect empty kubeconfig and use in-cluster config
		secretManifest = `apiVersion: v1
kind: Secret
metadata:
  name: harvester-kubeconfig
  namespace: butler-system
  annotations:
    butler.butlerlabs.dev/note: "Empty kubeconfig - provider will use in-cluster config"
type: Opaque
stringData:
  kubeconfig: ""
`
	}

	// Build harvester config section
	harvesterConfig := fmt.Sprintf(`    namespace: "%s"
    networkName: "%s"`, namespace, creds.NetworkName)

	// Add optional fields if provided
	if creds.ImageName != "" {
		harvesterConfig += fmt.Sprintf(`
    imageName: "%s"`, creds.ImageName)
	}
	if creds.StorageClassName != "" {
		harvesterConfig += fmt.Sprintf(`
    storageClassName: "%s"`, creds.StorageClassName)
	}

	providerConfigManifest := fmt.Sprintf(`apiVersion: butler.butlerlabs.dev/v1alpha1
kind: ProviderConfig
metadata:
  name: harvester
  namespace: butler-system
spec:
  provider: harvester
  credentialsRef:
    name: harvester-kubeconfig
    namespace: butler-system
  harvester:
%s
`, harvesterConfig)

	return secretManifest, providerConfigManifest
}

func (i *Installer) generateVSphereProviderConfig(creds *VSphereCredentials) (string, string) {
	secretManifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: vsphere-credentials
  namespace: butler-system
type: Opaque
stringData:
  VSPHERE_SERVER: "%s"
  VSPHERE_USER: "%s"
  VSPHERE_PASSWORD: "%s"
`, creds.Server, creds.Username, creds.Password)

	providerConfigManifest := fmt.Sprintf(`apiVersion: butler.butlerlabs.dev/v1alpha1
kind: ProviderConfig
metadata:
  name: vsphere
  namespace: butler-system
spec:
  type: vsphere
  vsphere:
    server: "%s"
    credentialsRef:
      name: vsphere-credentials
      namespace: butler-system
`, creds.Server)

	return secretManifest, providerConfigManifest
}

func (i *Installer) generateProxmoxProviderConfig(creds *ProxmoxCredentials) (string, string) {
	secretManifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: proxmox-credentials
  namespace: butler-system
type: Opaque
stringData:
  PROXMOX_URL: "%s"
  PROXMOX_USER: "%s"
  PROXMOX_PASSWORD: "%s"
`, creds.Endpoint, creds.Username, creds.Password)

	providerConfigManifest := fmt.Sprintf(`apiVersion: butler.butlerlabs.dev/v1alpha1
kind: ProviderConfig
metadata:
  name: proxmox
  namespace: butler-system
spec:
  type: proxmox
  proxmox:
    endpoint: "%s"
    credentialsRef:
      name: proxmox-credentials
      namespace: butler-system
`, creds.Endpoint)

	return secretManifest, providerConfigManifest
}

// InstallCAPI installs Cluster API with the specified infrastructure providers
func (i *Installer) InstallCAPI(ctx context.Context, kubeconfig []byte, version string, mgmtProvider string, additionalProviders []butlerv1alpha1.CAPIInfraProviderSpec, creds *ProviderCredentials) error {
	logger := log.FromContext(ctx)

	logger.Info("Installing CAPI", "version", version, "mgmtProvider", mgmtProvider)

	// Make kubeconfig insecure for clusterctl compatibility with self-signed certs
	insecureKubeconfig, err := makeKubeconfigInsecure(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to prepare kubeconfig: %w", err)
	}

	kubeconfigPath, cleanup, err := i.writeKubeconfig(insecureKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	defer cleanup()

	// Install CAPI core WITHOUT infrastructure providers
	// We install infra providers manually to avoid clusterctl env var requirements
	// NOTE: We still use --control-plane kamaji because:
	// 1. The Kamaji CAPI provider creates KamajiControlPlane -> TenantControlPlane
	// 2. We haven't built steward-capi-provider yet
	// 3. For butler-controller's tenant cluster creation, we'll need to either:
	//    a) Build steward-capi-provider (future work)
	//    b) Have butler-controller create Steward TenantControlPlanes directly
	// For now, this works because the management cluster's Steward installation
	// is independent of CAPI - CAPI is for tenant cluster creation.
	args := []string{
		"init",
		"--kubeconfig", kubeconfigPath,
		"--bootstrap", "kubeadm",
		// "--control-plane", "steward:v0.1.0", // TODO: Replace with steward-capi-provider when built
		// NO --infrastructure flag - we install providers manually
	}

	logger.Info("Running clusterctl init (core components only)")

	cmd := exec.CommandContext(ctx, "clusterctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("clusterctl init failed: %w, output: %s", err, string(output))
	}

	logger.Info("CAPI core installed successfully")

	// Install Steward CAPI provider manually
	// Use server-side apply to avoid annotation size limit (CRD is too large for client-side apply)
	logger.Info("Installing Steward CAPI provider manually")
	stewardURL := "https://github.com/butlerdotdev/cluster-api-control-plane-provider-steward/releases/download/v0.1.0/control-plane-components.yaml"
	if err := i.runKubectl(ctx, kubeconfigPath, "apply", "--server-side", "--force-conflicts", "-f", stewardURL); err != nil {
		return fmt.Errorf("failed to install Steward CAPI provider: %w", err)
	}
	// Install infrastructure provider manually with credentials
	if err := i.installInfraProvider(ctx, kubeconfigPath, mgmtProvider, creds); err != nil {
		return fmt.Errorf("failed to install %s provider: %w", mgmtProvider, err)
	}

	// Install any additional providers
	for _, p := range additionalProviders {
		if p.Name != mgmtProvider {
			if err := i.installInfraProvider(ctx, kubeconfigPath, p.Name, creds); err != nil {
				logger.Info("Failed to install additional provider", "provider", p.Name, "error", err)
			}
		}
	}

	// Ensure CAPI namespaces have privileged PSA
	capiNamespaces, err := i.getCAPINamespaces(ctx, kubeconfigPath)
	if err != nil {
		logger.Info("Failed to list CAPI namespaces, using defaults", "error", err)
		capiNamespaces = []string{"capi-system", "capi-kubeadm-bootstrap-system"}
	}
	for _, ns := range capiNamespaces {
		if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, ns); err != nil {
			logger.Info("Failed to set privileged PSA on namespace", "namespace", ns, "error", err)
		}
	}

	// Wait for controllers
	if err := i.waitForCAPIReady(ctx, kubeconfigPath); err != nil {
		return fmt.Errorf("CAPI controllers not ready: %w", err)
	}

	return nil
}

// installInfraProvider installs a CAPI infrastructure provider
func (i *Installer) installInfraProvider(ctx context.Context, kubeconfigPath string, provider string, creds *ProviderCredentials) error {
	logger := log.FromContext(ctx)

	var providerURL string
	var namespace string

	switch provider {
	case "harvester":
		namespace = "capk-system"
		providerURL = "https://github.com/kubernetes-sigs/cluster-api-provider-kubevirt/releases/download/v0.1.9/infrastructure-components.yaml"
	case "nutanix":
		namespace = "capx-system"
		providerURL = "https://github.com/nutanix-cloud-native/cluster-api-provider-nutanix/releases/download/v1.4.0/infrastructure-components.yaml"
	case "vsphere":
		namespace = "capv-system"
		providerURL = "https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/releases/download/v1.11.0/infrastructure-components.yaml"
	case "proxmox":
		namespace = "capmox-system"
		providerURL = "https://github.com/ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.6.0/infrastructure-components.yaml"
	default:
		logger.Info("Unknown provider, skipping", "provider", provider)
		return nil
	}

	logger.Info("Installing infrastructure provider", "provider", provider, "namespace", namespace)

	// Ensure namespace exists and is privileged
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, namespace); err != nil {
		logger.Info("Failed to prepare provider namespace", "namespace", namespace, "error", err)
	}

	// Download manifest
	resp, err := http.Get(providerURL)
	if err != nil {
		return fmt.Errorf("failed to download provider manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download provider manifest: HTTP %d", resp.StatusCode)
	}

	manifestBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read provider manifest: %w", err)
	}

	// Substitute template variables with actual credentials
	manifest := string(manifestBytes)
	if provider == "nutanix" && creds != nil && creds.Nutanix != nil {
		manifest = i.substituteNutanixCredentials(manifest, creds.Nutanix)
	}

	// Write processed manifest to temp file
	tmpFile, err := os.CreateTemp("", "provider-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(manifest); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	tmpFile.Close()

	// Apply manifest
	if err := i.runKubectl(ctx, kubeconfigPath, "apply", "-f", tmpFile.Name()); err != nil {
		return fmt.Errorf("failed to apply provider manifests: %w", err)
	}

	logger.Info("Infrastructure provider installed", "provider", provider)
	return nil
}

// substituteNutanixCredentials replaces template variables with actual values
func (i *Installer) substituteNutanixCredentials(manifest string, creds *NutanixCredentials) string {
	port := creds.Port
	if port == "" {
		port = "9440"
	}

	insecure := "false"
	if creds.Insecure {
		insecure = "true"
	}

	// Build the full endpoint URL if not already a URL
	endpoint := creds.Endpoint
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		endpoint = fmt.Sprintf("https://%s:%s", creds.Endpoint, port)
	}

	// Replace template variables (plain text)
	replacements := map[string]string{
		"${NUTANIX_ENDPOINT}": endpoint,
		"${NUTANIX_USER}":     creds.Username,
		"${NUTANIX_PASSWORD}": creds.Password,
		"${NUTANIX_PORT}":     port,
		"${NUTANIX_INSECURE}": insecure,
		// Variables with default values - use the defaults
		"${NUTANIX_INSECURE=false}":             insecure,
		"${NUTANIX_PORT=9440}":                  port,
		"${NUTANIX_ADDITIONAL_TRUST_BUNDLE=''}": "",
		"${NUTANIX_LOG_DEVELOPMENT=true}":       "true",
		"${NUTANIX_LOG_LEVEL=info}":             "info",
		"${NUTANIX_LOG_STACKTRACE_LEVEL=panic}": "panic",
	}

	for placeholder, value := range replacements {
		manifest = strings.ReplaceAll(manifest, placeholder, value)
	}

	return manifest
}

// waitForCAPIReady waits for CAPI controllers to be ready
func (i *Installer) waitForCAPIReady(ctx context.Context, kubeconfigPath string) error {
	logger := log.FromContext(ctx)

	namespaces, err := i.getCAPINamespaces(ctx, kubeconfigPath)
	if err != nil {
		logger.Info("Failed to list CAPI namespaces, using defaults", "error", err)
		namespaces = []string{"capi-system", "capi-kubeadm-bootstrap-system"}
	}

	for _, ns := range namespaces {
		args := []string{
			"--kubeconfig", kubeconfigPath,
			"wait", "--for=condition=Available",
			"deployment", "--all",
			"-n", ns,
			"--timeout=5m",
		}
		cmd := exec.CommandContext(ctx, "kubectl", args...)
		if err := cmd.Run(); err != nil {
			logger.Info("Waiting for CAPI namespace", "namespace", ns, "error", err)
			continue
		}
		logger.Info("CAPI namespace ready", "namespace", ns)
	}
	return nil
}

// getCAPINamespaces returns all namespaces that are CAPI-related
func (i *Installer) getCAPINamespaces(ctx context.Context, kubeconfigPath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfigPath, "--insecure-skip-tls-verify", "get", "ns", "-o", "jsonpath={.items[*].metadata.name}")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var capiNamespaces []string
	for _, ns := range strings.Split(string(output), " ") {
		ns = strings.TrimSpace(ns)
		if strings.HasPrefix(ns, "capi") || strings.HasPrefix(ns, "cap") {
			capiNamespaces = append(capiNamespaces, ns)
		}
	}
	return capiNamespaces, nil
}

// InstallButlerController installs butler-controller on the management cluster
func (i *Installer) InstallButlerController(ctx context.Context, kubeconfig []byte, image string) error {
	logger := log.FromContext(ctx)
	logger.Info("Installing butler-controller", "image", image)

	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	defer cleanup()

	// Ensure namespace exists first
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "butler-system"); err != nil {
		return fmt.Errorf("failed to create butler-system namespace: %w", err)
	}

	manifest := i.generateButlerControllerManifest(image)

	tmpFile, err := os.CreateTemp("", "butler-controller-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(manifest); err != nil {
		return err
	}
	tmpFile.Close()

	if err := i.runKubectl(ctx, kubeconfigPath, "apply", "-f", tmpFile.Name()); err != nil {
		return fmt.Errorf("failed to apply butler-controller: %w", err)
	}

	// Wait for deployment
	args := []string{
		"rollout", "status", "deployment", "butler-controller",
		"-n", "butler-system",
		"--timeout=3m",
	}

	if err := i.runKubectl(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("butler-controller not ready: %w", err)
	}

	logger.Info("butler-controller installed successfully")
	return nil
}

// generateButlerControllerManifest generates the deployment manifest
func (i *Installer) generateButlerControllerManifest(image string) string {
	return fmt.Sprintf(`---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: butler-controller
  namespace: butler-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: butler-controller-role
rules:
# Butler CRDs
- apiGroups: ["butler.butlerlabs.dev"]
  resources: ["*"]
  verbs: ["*"]
# CAPI resources
- apiGroups: ["cluster.x-k8s.io"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["infrastructure.cluster.x-k8s.io"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["controlplane.cluster.x-k8s.io"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["bootstrap.cluster.x-k8s.io"]
  resources: ["*"]
  verbs: ["*"]
# Steward resources (hosted control planes)
- apiGroups: ["steward.butlerlabs.dev"]
  resources: ["*"]
  verbs: ["*"]
# Legacy Kamaji resources (for CAPI provider compatibility during transition)
- apiGroups: ["kamaji.clastix.io"]
  resources: ["*"]
  verbs: ["*"]
# Core resources - needed for Helm chart installs in ANY namespace
- apiGroups: [""]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["apps"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["batch"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["networking.k8s.io"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["policy"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["autoscaling"]
  resources: ["*"]
  verbs: ["*"]
# Gateway API resources
- apiGroups: ["gateway.networking.k8s.io"]
  resources: ["*"]
  verbs: ["*"]
# Flux
- apiGroups: ["helm.toolkit.fluxcd.io"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: ["source.toolkit.fluxcd.io"]
  resources: ["*"]
  verbs: ["*"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: butler-controller-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: butler-controller-role
subjects:
- kind: ServiceAccount
  name: butler-controller
  namespace: butler-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: butler-controller-leader-election
  namespace: butler-system
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: butler-controller-leader-election
  namespace: butler-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: butler-controller-leader-election
subjects:
- kind: ServiceAccount
  name: butler-controller
  namespace: butler-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: butler-controller
  namespace: butler-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: butler-controller
  template:
    metadata:
      labels:
        app: butler-controller
    spec:
      serviceAccountName: butler-controller
      containers:
      - name: manager
        image: %s
        args:
        - --leader-elect
        ports:
        - containerPort: 8080
        - containerPort: 8081
        resources:
          limits:
            cpu: 500m
            memory: 256Mi
          requests:
            cpu: 100m
            memory: 128Mi
`, image)
}

// InstallConsole installs butler-console (server + frontend) on the management cluster
// Returns the console URL for user output
func (i *Installer) InstallConsole(ctx context.Context, kubeconfig []byte, spec *butlerv1alpha1.ConsoleAddonSpec, clusterName string) (string, error) {
	logger := log.FromContext(ctx)

	version := "0.1.0"
	if spec != nil && spec.Version != "" {
		version = spec.Version
	}

	logger.Info("Installing butler-console", "version", version)

	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return "", fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	defer cleanup()

	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "butler-system"); err != nil {
		return "", fmt.Errorf("failed to prepare butler-system namespace: %w", err)
	}

	// Build helm values
	values := []string{
		"server.image.tag=latest",
		"frontend.image.tag=latest",
	}

	// Configure ingress if enabled
	if spec != nil && spec.Ingress != nil && spec.Ingress.Enabled {
		host := spec.Ingress.Host
		if host == "" {
			host = fmt.Sprintf("butler.%s.local", clusterName)
		}
		values = append(values,
			"ingress.enabled=true",
			"ingress.hosts[0].host="+host,
			"ingress.hosts[0].paths[0].path=/api",
			"ingress.hosts[0].paths[0].pathType=Prefix",
			"ingress.hosts[0].paths[0].service=server",
			"ingress.hosts[0].paths[1].path=/ws",
			"ingress.hosts[0].paths[1].pathType=Prefix",
			"ingress.hosts[0].paths[1].service=server",
			"ingress.hosts[0].paths[2].path=/",
			"ingress.hosts[0].paths[2].pathType=Prefix",
			"ingress.hosts[0].paths[2].service=frontend",
		)
		if spec.Ingress.ClassName != "" {
			values = append(values, "ingress.className="+spec.Ingress.ClassName)
		}
		if spec.Ingress.TLS {
			secretName := spec.Ingress.TLSSecretName
			if secretName == "" {
				secretName = "butler-console-tls"
			}
			values = append(values,
				"ingress.tls[0].secretName="+secretName,
				"ingress.tls[0].hosts[0]="+host,
			)
		}
	}

	// Build helm install args - USE OCI REGISTRY
	args := []string{
		"upgrade", "--install",
		"butler-console",
		"oci://ghcr.io/butlerdotdev/charts/butler-console",
		"--version", version,
		"-n", "butler-system",
		"--wait",
		"--timeout", "5m",
	}
	for _, v := range values {
		args = append(args, "--set", v)
	}

	logger.Info("Installing butler-console via helm", "values", values)

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return "", fmt.Errorf("failed to install butler-console: %w", err)
	}

	// Wait for deployments
	if err := i.runKubectl(ctx, kubeconfigPath,
		"rollout", "status", "deployment", "butler-console-server",
		"-n", "butler-system", "--timeout=3m"); err != nil {
		return "", fmt.Errorf("butler-console-server not ready: %w", err)
	}

	if err := i.runKubectl(ctx, kubeconfigPath,
		"rollout", "status", "deployment", "butler-console-frontend",
		"-n", "butler-system", "--timeout=3m"); err != nil {
		return "", fmt.Errorf("butler-console-frontend not ready: %w", err)
	}

	logger.Info("butler-console installed successfully")

	// Determine URL
	return i.getConsoleURLFromSpec(ctx, kubeconfigPath, spec, clusterName), nil
}

func (i *Installer) getConsoleURLFromSpec(ctx context.Context, kubeconfigPath string, spec *butlerv1alpha1.ConsoleAddonSpec, clusterName string) string {
	if spec != nil && spec.Ingress != nil && spec.Ingress.Enabled {
		host := spec.Ingress.Host
		if host == "" {
			host = fmt.Sprintf("butler.%s.local", clusterName)
		}
		scheme := "http"
		if spec.Ingress.TLS {
			scheme = "https"
		}
		return fmt.Sprintf("%s://%s", scheme, host)
	}

	// Try LoadBalancer IP
	cmd := exec.CommandContext(ctx, i.KubectlPath,
		"--kubeconfig", kubeconfigPath,
		"--insecure-skip-tls-verify",
		"get", "svc", "butler-console-frontend",
		"-n", "butler-system",
		"-o", "jsonpath={.status.loadBalancer.ingress[0].ip}",
	)
	if output, err := cmd.Output(); err == nil && len(output) > 0 {
		return fmt.Sprintf("http://%s", string(output))
	}

	return "kubectl port-forward -n butler-system svc/butler-console-frontend 3000:80"
}

// getConsoleURL determines the URL to access the console
func (i *Installer) getConsoleURL(ctx context.Context, kubeconfigPath string, cfg ConsoleConfig) string {
	logger := log.FromContext(ctx)

	// If ingress is configured, use the ingress host
	if cfg.Ingress.Enabled && cfg.Ingress.Host != "" {
		scheme := "http"
		if cfg.Ingress.TLS {
			scheme = "https"
		}
		return fmt.Sprintf("%s://%s", scheme, cfg.Ingress.Host)
	}

	// Try to get LoadBalancer IP from frontend service
	cmd := exec.CommandContext(ctx, i.KubectlPath,
		"--kubeconfig", kubeconfigPath,
		"--insecure-skip-tls-verify",
		"get", "svc", "butler-console-frontend",
		"-n", "butler-system",
		"-o", "jsonpath={.status.loadBalancer.ingress[0].ip}",
	)
	if i.NodeIP != "" {
		cmd.Args = append(cmd.Args, "--server", fmt.Sprintf("https://%s:6443", i.NodeIP))
	}

	output, err := cmd.Output()
	if err == nil && len(output) > 0 {
		return fmt.Sprintf("http://%s", string(output))
	}

	// Fallback: try to get external IP
	cmd = exec.CommandContext(ctx, i.KubectlPath,
		"--kubeconfig", kubeconfigPath,
		"--insecure-skip-tls-verify",
		"get", "svc", "butler-console-frontend",
		"-n", "butler-system",
		"-o", "jsonpath={.spec.externalIPs[0]}",
	)
	if i.NodeIP != "" {
		cmd.Args = append(cmd.Args, "--server", fmt.Sprintf("https://%s:6443", i.NodeIP))
	}

	output, err = cmd.Output()
	if err == nil && len(output) > 0 {
		return fmt.Sprintf("http://%s", string(output))
	}

	// Last resort: use port-forward instruction
	logger.Info("Could not determine console external URL, will require port-forward")
	return "kubectl port-forward -n butler-system svc/butler-console-frontend 3000:80"
}

// ConsoleConfig is passed to InstallConsole
// This mirrors the config from orchestrator package
type ConsoleConfig struct {
	Enabled bool
	Version string
	Ingress ConsoleIngressConfig
	Auth    ConsoleAuthConfig
}

type ConsoleIngressConfig struct {
	Enabled       bool
	Host          string
	ClassName     string
	TLS           bool
	TLSSecretName string
}

type ConsoleAuthConfig struct {
	AdminPassword string
	JWTSecret     string
}
