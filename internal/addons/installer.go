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
	Endpoint string
	Username string
	Password string
	Port     string
	Insecure bool
}

// HarvesterCredentials holds Harvester credentials
type HarvesterCredentials struct {
	// Harvester uses kubeconfig, no additional creds needed for CAPK
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
func (i *Installer) InstallMetalLB(ctx context.Context, kubeconfig []byte, addressPool string) error {
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

// InstallKamaji installs Kamaji for hosted control planes
func (i *Installer) InstallKamaji(ctx context.Context, kubeconfig []byte, version string) error {
	logger := log.FromContext(ctx)
	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if version == "" {
		version = "1.0.0"
	}

	logger.Info("Installing Kamaji", "version", version)

	// Ensure namespace exists and is privileged
	if err := i.ensurePrivilegedNamespace(ctx, kubeconfigPath, "kamaji-system"); err != nil {
		return fmt.Errorf("failed to prepare kamaji-system namespace: %w", err)
	}

	// Add Clastix repo
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "add", "clastix", "https://clastix.github.io/charts"); err != nil {
		logger.Info("helm repo add failed (may already exist)", "error", err)
	}
	if err := i.runHelm(ctx, kubeconfigPath, "repo", "update"); err != nil {
		logger.Info("helm repo update failed", "error", err)
	}

	args := []string{
		"upgrade", "--install", "kamaji", "clastix/kamaji",
		"--namespace", "kamaji-system",
		"--version", version,
		"--set", "etcd.deploy=true",
		"--wait",
		"--timeout", "5m",
	}

	if err := i.runHelm(ctx, kubeconfigPath, args...); err != nil {
		return fmt.Errorf("failed to install Kamaji: %w", err)
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

	// TODO: Install Butler CRDs and controllers
	// For now, just ensure namespace exists
	logger.Info("Butler namespace created - full installation pending")

	return nil
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
	args := []string{
		"init",
		"--kubeconfig", kubeconfigPath,
		"--bootstrap", "kubeadm",
		"--control-plane", "kamaji",
		// NO --infrastructure flag - we install providers manually
	}

	logger.Info("Running clusterctl init (core components only)")

	cmd := exec.CommandContext(ctx, "clusterctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("clusterctl init failed: %w, output: %s", err, string(output))
	}

	logger.Info("CAPI core installed successfully")

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
- apiGroups: ["butler.butlerlabs.dev"]
  resources: ["*"]
  verbs: ["*"]
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
- apiGroups: ["kamaji.clastix.io"]
  resources: ["*"]
  verbs: ["*"]
- apiGroups: [""]
  resources: ["secrets", "configmaps", "namespaces", "services", "events"]
  verbs: ["*"]
- apiGroups: ["helm.toolkit.fluxcd.io"]
  resources: ["helmreleases"]
  verbs: ["*"]
- apiGroups: ["source.toolkit.fluxcd.io"]
  resources: ["helmrepositories", "helmcharts"]
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
