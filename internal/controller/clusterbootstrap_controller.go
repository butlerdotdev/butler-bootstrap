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

package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	butlerv1alpha1 "github.com/butlerdotdev/butler-api/api/v1alpha1"
	"github.com/butlerdotdev/butler-bootstrap/internal/addons"
)

const (
	clusterBootstrapFinalizer = "clusterbootstrap.butler.butlerlabs.dev/finalizer"
	// Requeue intervals
	requeueShort  = 5 * time.Second
	requeueMedium = 15 * time.Second
)

// ClusterBootstrapReconciler reconciles a ClusterBootstrap object
type ClusterBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// TalosClient handles Talos operations
	TalosClient TalosClientInterface
	// AddonInstaller handles addon installations
	AddonInstaller AddonInstallerInterface
}

// TalosClientInterface defines the interface for Talos operations
type TalosClientInterface interface {
	GenerateConfig(ctx context.Context, opts TalosConfigOptions) (*TalosConfigs, error)
	ApplyConfig(ctx context.Context, nodeIP string, config []byte, insecure bool) error
	Bootstrap(ctx context.Context, nodeIP string) error
	GetKubeconfig(ctx context.Context, nodeIP string) ([]byte, error)
	WaitForNodeReady(ctx context.Context, nodeIP string, timeout time.Duration) error
	SetTalosConfigFromBytes(config []byte) error
}

// TalosConfigOptions defines options for generating Talos configs
type TalosConfigOptions struct {
	ClusterName     string
	ControlPlaneVIP string
	ControlPlaneIPs []string
	WorkerIPs       []string
	PodCIDR         string
	ServiceCIDR     string
	TalosVersion    string
	InstallDisk     string
	ConfigPatches   []ConfigPatch
}

// ConfigPatch mirrors the CRD type
type ConfigPatch struct {
	Op    string
	Path  string
	Value string
}

// TalosConfigs contains generated Talos configurations
type TalosConfigs struct {
	TalosConfig        []byte
	ControlPlaneConfig []byte
	WorkerConfig       []byte
	NodeConfigs        map[string][]byte
}

// AddonInstallerInterface defines the interface for addon installations
type AddonInstallerInterface interface {
	SetNodeIP(ip string)
	InstallKubeVip(ctx context.Context, kubeconfig []byte, vip string, iface string, version string) error
	InstallCilium(ctx context.Context, kubeconfig []byte, version string, hubbleEnabled bool) error
	InstallCertManager(ctx context.Context, kubeconfig []byte, version string) error
	InstallLonghorn(ctx context.Context, kubeconfig []byte, version string, replicaCount int32) error
	InstallMetalLB(ctx context.Context, kubeconfig []byte, addressPool string) error
	InstallTraefik(ctx context.Context, kubeconfig []byte, version string) error
	InstallKamaji(ctx context.Context, kubeconfig []byte, version string) error
	InstallFlux(ctx context.Context, kubeconfig []byte) error
	InstallButler(ctx context.Context, kubeconfig []byte) error
	InstallCAPI(ctx context.Context, kubeconfig []byte, version string, mgmtProvider string, additionalProviders []butlerv1alpha1.CAPIInfraProviderSpec, creds *addons.ProviderCredentials) error
	InstallButlerController(ctx context.Context, kubeconfig []byte, image string) error
}

// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=clusterbootstraps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=clusterbootstraps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=clusterbootstraps/finalizers,verbs=update
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=machinerequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=providerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update

func (r *ClusterBootstrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ClusterBootstrap instance
	cb := &butlerv1alpha1.ClusterBootstrap{}
	if err := r.Get(ctx, req.NamespacedName, cb); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get ClusterBootstrap")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if cb.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, cb)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(cb, clusterBootstrapFinalizer) {
		controllerutil.AddFinalizer(cb, clusterBootstrapFinalizer)
		if err := r.Update(ctx, cb); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Skip if paused
	if cb.Spec.Paused {
		logger.Info("ClusterBootstrap is paused, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Initialize phase if empty
	if cb.Status.Phase == "" {
		cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhasePending
		cb.Status.LastUpdated = metav1.Now()
		if err := r.Status().Update(ctx, cb); err != nil {
			logger.Error(err, "Failed to set initial phase")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Route based on phase
	switch cb.Status.Phase {
	case butlerv1alpha1.ClusterBootstrapPhasePending:
		return r.reconcilePending(ctx, cb)
	case butlerv1alpha1.ClusterBootstrapPhaseProvisioningMachines:
		return r.reconcileProvisioningMachines(ctx, cb)
	case butlerv1alpha1.ClusterBootstrapPhaseConfiguringTalos:
		return r.reconcileConfiguringTalos(ctx, cb)
	case butlerv1alpha1.ClusterBootstrapPhaseBootstrappingCluster:
		return r.reconcileBootstrappingCluster(ctx, cb)
	case butlerv1alpha1.ClusterBootstrapPhaseInstallingAddons:
		return r.reconcileInstallingAddons(ctx, cb)
	case butlerv1alpha1.ClusterBootstrapPhasePivoting:
		return r.reconcilePivoting(ctx, cb)
	case butlerv1alpha1.ClusterBootstrapPhaseReady:
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	case butlerv1alpha1.ClusterBootstrapPhaseFailed:
		logger.Info("ClusterBootstrap is in Failed state")
		return ctrl.Result{}, nil
	default:
		logger.Info("Unknown phase", "phase", cb.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *ClusterBootstrapReconciler) reconcileDelete(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ClusterBootstrap deletion")

	// Delete all MachineRequests created by this bootstrap
	machineRequests := &butlerv1alpha1.MachineRequestList{}
	if err := r.List(ctx, machineRequests, client.InNamespace(cb.Namespace), client.MatchingLabels{
		"butler.butlerlabs.dev/cluster-bootstrap": cb.Name,
	}); err != nil {
		logger.Error(err, "Failed to list MachineRequests")
		return ctrl.Result{}, err
	}

	for _, mr := range machineRequests.Items {
		if mr.DeletionTimestamp == nil {
			if err := r.Delete(ctx, &mr); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "Failed to delete MachineRequest", "name", mr.Name)
				return ctrl.Result{}, err
			}
			logger.Info("Deleted MachineRequest", "name", mr.Name)
		}
	}

	// Wait for all to be deleted
	if len(machineRequests.Items) > 0 {
		hasPending := false
		for _, mr := range machineRequests.Items {
			if mr.DeletionTimestamp == nil {
				hasPending = true
				break
			}
		}
		if hasPending {
			logger.Info("Waiting for MachineRequests to be deleted")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(cb, clusterBootstrapFinalizer)
	if err := r.Update(ctx, cb); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("ClusterBootstrap deletion complete")
	return ctrl.Result{}, nil
}

func (r *ClusterBootstrapReconciler) reconcilePending(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Pending phase")

	// Validate ProviderConfig exists
	providerConfig := &butlerv1alpha1.ProviderConfig{}
	providerNS := cb.Spec.ProviderRef.Namespace
	if providerNS == "" {
		providerNS = cb.Namespace
	}

	if err := r.Get(ctx, client.ObjectKey{
		Name:      cb.Spec.ProviderRef.Name,
		Namespace: providerNS,
	}, providerConfig); err != nil {
		if errors.IsNotFound(err) {
			r.setFailure(cb, "ProviderConfigNotFound", fmt.Sprintf("ProviderConfig %s/%s not found", providerNS, cb.Spec.ProviderRef.Name))
			r.Status().Update(ctx, cb)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Transition to ProvisioningMachines
	cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhaseProvisioningMachines
	cb.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Transitioning to ProvisioningMachines phase")
	return ctrl.Result{Requeue: true}, nil
}

func (r *ClusterBootstrapReconciler) reconcileProvisioningMachines(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ProvisioningMachines phase")

	clusterName := cb.Spec.Cluster.Name

	// Create control plane MachineRequests
	for i := int32(0); i < cb.Spec.Cluster.ControlPlane.Replicas; i++ {
		mrName := fmt.Sprintf("%s-cp-%d", clusterName, i)
		if err := r.ensureMachineRequest(ctx, cb, mrName, "control-plane", cb.Spec.Cluster.ControlPlane); err != nil {
			logger.Error(err, "Failed to ensure control plane MachineRequest", "name", mrName)
			return ctrl.Result{}, err
		}
	}

	// Create worker MachineRequests if specified
	if cb.Spec.Cluster.Workers != nil {
		for i := int32(0); i < cb.Spec.Cluster.Workers.Replicas; i++ {
			mrName := fmt.Sprintf("%s-worker-%d", clusterName, i)
			if err := r.ensureMachineRequest(ctx, cb, mrName, "worker", *cb.Spec.Cluster.Workers); err != nil {
				logger.Error(err, "Failed to ensure worker MachineRequest", "name", mrName)
				return ctrl.Result{}, err
			}
		}
	}

	// Update machine statuses
	if err := r.updateMachineStatuses(ctx, cb); err != nil {
		logger.Error(err, "Failed to update machine statuses")
		return ctrl.Result{}, err
	}

	// Check if all machines are ready
	if r.allMachinesRunning(cb) {
		logger.Info("All machines are running, transitioning to ConfiguringTalos")
		cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhaseConfiguringTalos
		cb.Status.LastUpdated = metav1.Now()
		if err := r.Status().Update(ctx, cb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Check for failed machines
	for _, m := range cb.Status.Machines {
		if m.Phase == string(butlerv1alpha1.MachinePhaseFailed) {
			r.setFailure(cb, "MachineProvisioningFailed", fmt.Sprintf("Machine %s failed to provision", m.Name))
			r.Status().Update(ctx, cb)
			return ctrl.Result{}, nil
		}
	}

	logger.Info("Waiting for machines to be ready", "count", len(cb.Status.Machines))
	return ctrl.Result{RequeueAfter: requeueMedium}, nil
}

func (r *ClusterBootstrapReconciler) ensureMachineRequest(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap, name string, role string, pool butlerv1alpha1.ClusterBootstrapNodePool) error {
	logger := log.FromContext(ctx)

	mr := &butlerv1alpha1.MachineRequest{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: cb.Namespace}, mr)
	if err == nil {
		return nil // Already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	// Create new MachineRequest
	mr = &butlerv1alpha1.MachineRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cb.Namespace,
			Labels: map[string]string{
				"butler.butlerlabs.dev/cluster-bootstrap": cb.Name,
				"butler.butlerlabs.dev/cluster":           cb.Spec.Cluster.Name,
				"butler.butlerlabs.dev/role":              role,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         cb.APIVersion,
					Kind:               cb.Kind,
					Name:               cb.Name,
					UID:                cb.UID,
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				},
			},
		},
		Spec: butlerv1alpha1.MachineRequestSpec{
			ProviderRef: cb.Spec.ProviderRef,
			MachineName: name,
			Role:        butlerv1alpha1.MachineRole(role),
			CPU:         pool.CPU,
			MemoryMB:    pool.MemoryMB,
			DiskGB:      pool.DiskGB,
			Labels:      pool.Labels,
		},
	}

	if err := r.Create(ctx, mr); err != nil {
		return err
	}

	logger.Info("Created MachineRequest", "name", name, "role", role)
	return nil
}

func (r *ClusterBootstrapReconciler) updateMachineStatuses(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) error {
	machineRequests := &butlerv1alpha1.MachineRequestList{}
	if err := r.List(ctx, machineRequests, client.InNamespace(cb.Namespace), client.MatchingLabels{
		"butler.butlerlabs.dev/cluster-bootstrap": cb.Name,
	}); err != nil {
		return err
	}

	// Build new status list preserving TalosConfigured and Ready flags
	oldMachines := make(map[string]butlerv1alpha1.ClusterBootstrapMachineStatus)
	for _, m := range cb.Status.Machines {
		oldMachines[m.Name] = m
	}

	cb.Status.Machines = nil
	for _, mr := range machineRequests.Items {
		role := mr.Labels["butler.butlerlabs.dev/role"]

		status := butlerv1alpha1.ClusterBootstrapMachineStatus{
			Name:      mr.Name,
			Role:      role,
			Phase:     string(mr.Status.Phase),
			IPAddress: mr.Status.IPAddress,
		}

		// Preserve flags from previous status
		if old, ok := oldMachines[mr.Name]; ok {
			status.TalosConfigured = old.TalosConfigured
			status.Ready = old.Ready
		}

		cb.Status.Machines = append(cb.Status.Machines, status)
	}

	return r.Status().Update(ctx, cb)
}

func (r *ClusterBootstrapReconciler) reconcileConfiguringTalos(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ConfiguringTalos phase")

	// Check if we need to generate configs
	if cb.Status.TalosConfig == "" {
		cpIPs := r.getControlPlaneIPs(cb)
		workerIPs := r.getWorkerIPs(cb)

		opts := TalosConfigOptions{
			ClusterName:     cb.Spec.Cluster.Name,
			ControlPlaneVIP: cb.Spec.Network.VIP,
			ControlPlaneIPs: cpIPs,
			WorkerIPs:       workerIPs,
			PodCIDR:         cb.Spec.Network.PodCIDR,
			ServiceCIDR:     cb.Spec.Network.ServiceCIDR,
			TalosVersion:    cb.Spec.Talos.Version,
			InstallDisk:     cb.Spec.Talos.InstallDisk,
		}

		// Convert config patches
		for _, p := range cb.Spec.Talos.ConfigPatches {
			opts.ConfigPatches = append(opts.ConfigPatches, ConfigPatch{
				Op:    p.Op,
				Path:  p.Path,
				Value: p.Value,
			})
		}

		configs, err := r.TalosClient.GenerateConfig(ctx, opts)
		if err != nil {
			logger.Error(err, "Failed to generate Talos configs")
			r.setFailure(cb, "TalosConfigGenerationFailed", err.Error())
			r.Status().Update(ctx, cb)
			return ctrl.Result{}, nil
		}

		// Store talosconfig in status
		cb.Status.TalosConfig = base64.StdEncoding.EncodeToString(configs.TalosConfig)

		// Store configs in a secret
		if err := r.storeTalosConfigs(ctx, cb, configs); err != nil {
			logger.Error(err, "Failed to store Talos configs")
			return ctrl.Result{}, err
		}

		if err := r.Status().Update(ctx, cb); err != nil {
			return ctrl.Result{}, err
		}

		// Set talosconfig for client
		if err := r.TalosClient.SetTalosConfigFromBytes(configs.TalosConfig); err != nil {
			logger.Error(err, "Failed to set talosconfig")
			return ctrl.Result{}, err
		}

		logger.Info("Generated Talos configs")
	}

	// Get configs from secret
	configs, err := r.getTalosConfigs(ctx, cb)
	if err != nil {
		logger.Error(err, "Failed to get Talos configs")
		return ctrl.Result{}, err
	}

	// Set talosconfig for client
	if err := r.TalosClient.SetTalosConfigFromBytes(configs.TalosConfig); err != nil {
		logger.Error(err, "Failed to set talosconfig")
	}

	// Apply configs to nodes that haven't been configured
	updated := false
	for i := range cb.Status.Machines {
		m := &cb.Status.Machines[i]
		if m.TalosConfigured || m.IPAddress == "" {
			continue
		}

		var config []byte
		if m.Role == "control-plane" {
			config = configs.ControlPlaneConfig
		} else {
			config = configs.WorkerConfig
		}

		logger.Info("Applying Talos config", "machine", m.Name, "ip", m.IPAddress)
		if err := r.TalosClient.ApplyConfig(ctx, m.IPAddress, config, true); err != nil {
			logger.Error(err, "Failed to apply Talos config", "machine", m.Name)
			continue
		}

		m.TalosConfigured = true
		updated = true
		logger.Info("Applied Talos config", "machine", m.Name)
	}

	if updated {
		if err := r.Status().Update(ctx, cb); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check if all machines are configured
	allConfigured := true
	for _, m := range cb.Status.Machines {
		if !m.TalosConfigured {
			allConfigured = false
			break
		}
	}

	if allConfigured && len(cb.Status.Machines) > 0 {
		logger.Info("All machines configured, transitioning to BootstrappingCluster")
		cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhaseBootstrappingCluster
		cb.Status.LastUpdated = metav1.Now()
		if err := r.Status().Update(ctx, cb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

func (r *ClusterBootstrapReconciler) reconcileBootstrappingCluster(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling BootstrappingCluster phase")

	cpIPs := r.getControlPlaneIPs(cb)
	if len(cpIPs) == 0 {
		r.setFailure(cb, "NoControlPlaneNodes", "No control plane nodes available for bootstrap")
		r.Status().Update(ctx, cb)
		return ctrl.Result{}, nil
	}

	firstCP := cpIPs[0]

	// Initialize AddonsInstalled map if needed
	if cb.Status.AddonsInstalled == nil {
		cb.Status.AddonsInstalled = make(map[string]bool)
	}

	// Bootstrap only once
	if !cb.Status.AddonsInstalled["bootstrap-initiated"] {
		logger.Info("Running talosctl bootstrap", "node", firstCP)

		if err := r.TalosClient.Bootstrap(ctx, firstCP); err != nil {
			// Retry on transient errors (node still booting)
			if strings.Contains(err.Error(), "connection refused") ||
				strings.Contains(err.Error(), "connection reset") ||
				strings.Contains(err.Error(), "i/o timeout") {
				logger.Info("Bootstrap failed, node may still be starting, retrying", "error", err)
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			logger.Error(err, "Failed to bootstrap cluster")
			r.setFailure(cb, "BootstrapFailed", err.Error())
			r.Status().Update(ctx, cb)
			return ctrl.Result{}, nil
		}

		cb.Status.AddonsInstalled["bootstrap-initiated"] = true
		if err := r.Status().Update(ctx, cb); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Bootstrap initiated successfully")
	}

	// Get kubeconfig (retry until ready)
	if cb.Status.Kubeconfig == "" {
		kubeconfig, err := r.TalosClient.GetKubeconfig(ctx, firstCP)
		if err != nil {
			logger.Info("Waiting for kubeconfig to be available", "error", err)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		cb.Status.Kubeconfig = base64.StdEncoding.EncodeToString(kubeconfig)
		cb.Status.ControlPlaneEndpoint = fmt.Sprintf("https://%s:6443", cb.Spec.Network.VIP)
		if err := r.Status().Update(ctx, cb); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Kubeconfig retrieved successfully")
	}

	// Transition to InstallingAddons
	cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhaseInstallingAddons
	cb.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// ensureAddonsMap ensures the AddonsInstalled map is initialized
func (r *ClusterBootstrapReconciler) ensureAddonsMap(cb *butlerv1alpha1.ClusterBootstrap) {
	if cb.Status.AddonsInstalled == nil {
		cb.Status.AddonsInstalled = make(map[string]bool)
	}
}

// isAddonInstalled checks if an addon is installed (safely handles nil map)
func (r *ClusterBootstrapReconciler) isAddonInstalled(cb *butlerv1alpha1.ClusterBootstrap, name string) bool {
	if cb.Status.AddonsInstalled == nil {
		return false
	}
	return cb.Status.AddonsInstalled[name]
}

// setAddonInstalled marks an addon as installed (safely handles nil map)
func (r *ClusterBootstrapReconciler) setAddonInstalled(cb *butlerv1alpha1.ClusterBootstrap, name string) {
	r.ensureAddonsMap(cb)
	cb.Status.AddonsInstalled[name] = true
}

// isAddonEnabled checks if an addon is explicitly enabled (handles nil and pointer bools)
func isAddonEnabled(enabled *bool) bool {
	if enabled == nil {
		return true // Default to enabled
	}
	return *enabled
}

func (r *ClusterBootstrapReconciler) reconcileInstallingAddons(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling InstallingAddons phase")

	// Always ensure map is initialized at start
	r.ensureAddonsMap(cb)

	kubeconfig, _ := base64.StdEncoding.DecodeString(cb.Status.Kubeconfig)

	// Set node IP for direct access before VIP is available
	cpIPs := r.getControlPlaneIPs(cb)
	if len(cpIPs) > 0 {
		r.AddonInstaller.SetNodeIP(cpIPs[0])
	}

	addons := cb.Spec.Addons

	// 1. kube-vip - required for VIP to work
	if !r.isAddonInstalled(cb, "kube-vip") {
		logger.Info("Installing kube-vip")
		version := "v0.8.7"
		if addons.ControlPlaneHA != nil && addons.ControlPlaneHA.Version != "" {
			version = addons.ControlPlaneHA.Version
		}

		iface := cb.Spec.Network.VIPInterface
		if iface == "" {
			iface = r.getDefaultVIPInterface(cb.Spec.Provider)
		}

		if err := r.AddonInstaller.InstallKubeVip(ctx, kubeconfig, cb.Spec.Network.VIP, iface, version); err != nil {
			logger.Error(err, "Failed to install kube-vip")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}

		r.setAddonInstalled(cb, "kube-vip")
		logger.Info("kube-vip installed successfully")
		if err := r.Status().Update(ctx, cb); err != nil {
			logger.Info("Failed to update status after kube-vip install", "error", err)
		}
	}

	// 2. Cilium CNI
	if addons.CNI != nil && addons.CNI.Type == "cilium" {
		if !r.isAddonInstalled(cb, "cilium") {
			logger.Info("Installing Cilium")
			version := addons.CNI.Version
			hubble := addons.CNI.HubbleEnabled

			if err := r.AddonInstaller.InstallCilium(ctx, kubeconfig, version, hubble); err != nil {
				logger.Error(err, "Failed to install Cilium")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "cilium")
			logger.Info("Cilium installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Cilium install", "error", err)
			}
		}
	}

	// 3. cert-manager - needed by Traefik and Kamaji webhooks
	if addons.CertManager == nil || isAddonEnabled(addons.CertManager.Enabled) {
		if !r.isAddonInstalled(cb, "cert-manager") {
			logger.Info("Installing cert-manager")
			version := ""
			if addons.CertManager != nil && addons.CertManager.Version != "" {
				version = addons.CertManager.Version
			}

			if err := r.AddonInstaller.InstallCertManager(ctx, kubeconfig, version); err != nil {
				logger.Error(err, "Failed to install cert-manager")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "cert-manager")
			logger.Info("cert-manager installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after cert-manager install", "error", err)
			}
		}
	}

	// 4. Longhorn storage
	if addons.Storage != nil && addons.Storage.Type == "longhorn" {
		if !r.isAddonInstalled(cb, "longhorn") {
			logger.Info("Installing Longhorn")
			version := addons.Storage.Version
			replicas := addons.Storage.DefaultReplicaCount
			if replicas == 0 {
				replicas = 2
			}

			if err := r.AddonInstaller.InstallLonghorn(ctx, kubeconfig, version, replicas); err != nil {
				logger.Error(err, "Failed to install Longhorn")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "longhorn")
			logger.Info("Longhorn installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Longhorn install", "error", err)
			}
		}
	}

	// 5. MetalLB
	if addons.LoadBalancer != nil && addons.LoadBalancer.Type == "metallb" && addons.LoadBalancer.AddressPool != "" {
		if !r.isAddonInstalled(cb, "metallb") {
			logger.Info("Installing MetalLB")

			if err := r.AddonInstaller.InstallMetalLB(ctx, kubeconfig, addons.LoadBalancer.AddressPool); err != nil {
				logger.Error(err, "Failed to install MetalLB")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "metallb")
			logger.Info("MetalLB installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after MetalLB install", "error", err)
			}
		}
	}

	// 6. Traefik ingress - after MetalLB so it can get LoadBalancer IP
	if addons.Ingress == nil || isAddonEnabled(addons.Ingress.Enabled) {
		if !r.isAddonInstalled(cb, "traefik") {
			logger.Info("Installing Traefik")
			version := ""
			if addons.Ingress != nil && addons.Ingress.Version != "" {
				version = addons.Ingress.Version
			}

			if err := r.AddonInstaller.InstallTraefik(ctx, kubeconfig, version); err != nil {
				logger.Error(err, "Failed to install Traefik")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "traefik")
			logger.Info("Traefik installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Traefik install", "error", err)
			}
		}
	}

	// 7. Kamaji - hosted control planes
	if addons.ControlPlaneProvider == nil || isAddonEnabled(addons.ControlPlaneProvider.Enabled) {
		if !r.isAddonInstalled(cb, "kamaji") {
			logger.Info("Installing Kamaji")
			version := ""
			if addons.ControlPlaneProvider != nil && addons.ControlPlaneProvider.Version != "" {
				version = addons.ControlPlaneProvider.Version
			}

			if err := r.AddonInstaller.InstallKamaji(ctx, kubeconfig, version); err != nil {
				logger.Error(err, "Failed to install Kamaji")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "kamaji")
			logger.Info("Kamaji installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Kamaji install", "error", err)
			}
		}
	}

	// 8. CAPI
	if addons.IsCAPIEnabled() {
		if !r.isAddonInstalled(cb, "capi") {
			logger.Info("Installing CAPI")

			version := addons.GetCAPIVersion()
			mgmtProvider := string(cb.Spec.Provider)

			var additionalProviders []butlerv1alpha1.CAPIInfraProviderSpec
			if addons.CAPI != nil {
				additionalProviders = addons.CAPI.InfrastructureProviders
			}

			// Get provider credentials from ProviderConfig
			creds, err := r.getProviderCredentials(ctx, cb)
			if err != nil {
				logger.Error(err, "Failed to get provider credentials for CAPI")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			logger.Info("Installing CAPI", "version", version, "mgmtProvider", mgmtProvider)

			if err := r.AddonInstaller.InstallCAPI(ctx, kubeconfig, version, mgmtProvider, additionalProviders, creds); err != nil {
				logger.Error(err, "Failed to install CAPI")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "capi")
			logger.Info("CAPI installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after CAPI install", "error", err)
			}
		}
	}

	// 9. Flux
	if addons.GitOps != nil && addons.GitOps.Type == "flux" && isAddonEnabled(addons.GitOps.Enabled) {
		if !r.isAddonInstalled(cb, "flux") {
			logger.Info("Installing Flux")

			if err := r.AddonInstaller.InstallFlux(ctx, kubeconfig); err != nil {
				logger.Error(err, "Failed to install Flux")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "flux")
			logger.Info("Flux installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Flux install", "error", err)
			}
		}
	}

	// 10. Butler Controller
	if addons.IsButlerControllerEnabled() {
		if !r.isAddonInstalled(cb, "butler-controller") {
			logger.Info("Installing Butler Controller")

			image := addons.GetButlerControllerImage()
			logger.Info("Installing Butler Controller", "image", image)

			if err := r.AddonInstaller.InstallButlerController(ctx, kubeconfig, image); err != nil {
				logger.Error(err, "Failed to install Butler Controller")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "butler-controller")
			logger.Info("Butler Controller installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Butler Controller install", "error", err)
			}
		}
	}

	// 11. Butler
	if !r.isAddonInstalled(cb, "butler") {
		logger.Info("Installing Butler components")

		if err := r.AddonInstaller.InstallButler(ctx, kubeconfig); err != nil {
			logger.Error(err, "Failed to install Butler")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}

		r.setAddonInstalled(cb, "butler")
		logger.Info("Butler installed successfully")
		if err := r.Status().Update(ctx, cb); err != nil {
			logger.Info("Failed to update status after Butler install", "error", err)
		}
	}

	// Transition to Pivoting
	logger.Info("All addons installed, transitioning to Pivoting")
	cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhasePivoting
	cb.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *ClusterBootstrapReconciler) reconcilePivoting(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Pivoting phase")

	// TODO: Implement pivot logic - for now just transition to Ready
	cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhaseReady
	cb.Status.LastUpdated = metav1.Now()
	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("ClusterBootstrap complete")
	return ctrl.Result{}, nil
}

// Helper functions

func (r *ClusterBootstrapReconciler) storeTalosConfigs(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap, configs *TalosConfigs) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-talos-configs", cb.Name),
			Namespace: cb.Namespace,
			Labels: map[string]string{
				"butler.butlerlabs.dev/cluster-bootstrap": cb.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         cb.APIVersion,
					Kind:               cb.Kind,
					Name:               cb.Name,
					UID:                cb.UID,
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				},
			},
		},
		Data: map[string][]byte{
			"talosconfig":       configs.TalosConfig,
			"controlplane.yaml": configs.ControlPlaneConfig,
			"worker.yaml":       configs.WorkerConfig,
		},
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: secret.Name, Namespace: secret.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, secret)
	}
	if err != nil {
		return err
	}

	existing.Data = secret.Data
	return r.Update(ctx, existing)
}

func (r *ClusterBootstrapReconciler) getTalosConfigs(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (*TalosConfigs, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      fmt.Sprintf("%s-talos-configs", cb.Name),
		Namespace: cb.Namespace,
	}, secret); err != nil {
		return nil, err
	}

	return &TalosConfigs{
		TalosConfig:        secret.Data["talosconfig"],
		ControlPlaneConfig: secret.Data["controlplane.yaml"],
		WorkerConfig:       secret.Data["worker.yaml"],
	}, nil
}

func (r *ClusterBootstrapReconciler) setFailure(cb *butlerv1alpha1.ClusterBootstrap, reason, message string) {
	cb.Status.Phase = butlerv1alpha1.ClusterBootstrapPhaseFailed
	cb.Status.FailureReason = reason
	cb.Status.FailureMessage = message
	cb.Status.LastUpdated = metav1.Now()
}

func (r *ClusterBootstrapReconciler) allMachinesRunning(cb *butlerv1alpha1.ClusterBootstrap) bool {
	expectedCount := int(cb.Spec.Cluster.ControlPlane.Replicas)
	if cb.Spec.Cluster.Workers != nil {
		expectedCount += int(cb.Spec.Cluster.Workers.Replicas)
	}

	if len(cb.Status.Machines) != expectedCount {
		return false
	}

	for _, m := range cb.Status.Machines {
		if m.Phase != string(butlerv1alpha1.MachinePhaseRunning) || m.IPAddress == "" {
			return false
		}
	}

	return true
}

func (r *ClusterBootstrapReconciler) getControlPlaneIPs(cb *butlerv1alpha1.ClusterBootstrap) []string {
	var ips []string
	for _, m := range cb.Status.Machines {
		if m.Role == "control-plane" && m.IPAddress != "" {
			ips = append(ips, m.IPAddress)
		}
	}
	return ips
}

func (r *ClusterBootstrapReconciler) getWorkerIPs(cb *butlerv1alpha1.ClusterBootstrap) []string {
	var ips []string
	for _, m := range cb.Status.Machines {
		if m.Role == "worker" && m.IPAddress != "" {
			ips = append(ips, m.IPAddress)
		}
	}
	return ips
}

// getDefaultVIPInterface returns the default network interface name for kube-vip based on provider
func (r *ClusterBootstrapReconciler) getDefaultVIPInterface(provider string) string {
	switch provider {
	case "nutanix":
		return "ens3"
	case "harvester":
		return "enp1s0"
	case "proxmox":
		return "eth0"
	default:
		return "eth0"
	}
}

// getProviderCredentials fetches credentials from the ProviderConfig's Secret
func (r *ClusterBootstrapReconciler) getProviderCredentials(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (*addons.ProviderCredentials, error) {
	logger := log.FromContext(ctx)

	// Get ProviderConfig
	providerConfig := &butlerv1alpha1.ProviderConfig{}
	providerNS := cb.Spec.ProviderRef.Namespace
	if providerNS == "" {
		providerNS = cb.Namespace
	}

	if err := r.Get(ctx, client.ObjectKey{
		Name:      cb.Spec.ProviderRef.Name,
		Namespace: providerNS,
	}, providerConfig); err != nil {
		return nil, fmt.Errorf("failed to get ProviderConfig: %w", err)
	}

	// Get credentials Secret
	secret := &corev1.Secret{}
	secretNS := providerConfig.Spec.CredentialsRef.Namespace
	if secretNS == "" {
		secretNS = providerConfig.Namespace
	}

	if err := r.Get(ctx, client.ObjectKey{
		Name:      providerConfig.Spec.CredentialsRef.Name,
		Namespace: secretNS,
	}, secret); err != nil {
		return nil, fmt.Errorf("failed to get credentials Secret: %w", err)
	}

	creds := &addons.ProviderCredentials{}

	switch cb.Spec.Provider {
	case "nutanix":
		creds.Nutanix = &addons.NutanixCredentials{
			Endpoint: string(secret.Data["endpoint"]),
			Username: string(secret.Data["username"]),
			Password: string(secret.Data["password"]),
			Port:     string(secret.Data["port"]),
			Insecure: string(secret.Data["insecure"]) == "true",
		}
		logger.Info("Retrieved Nutanix credentials", "endpoint", creds.Nutanix.Endpoint)
	case "harvester":
		creds.Harvester = &addons.HarvesterCredentials{}
	case "vsphere":
		creds.VSphere = &addons.VSphereCredentials{
			Server:   string(secret.Data["server"]),
			Username: string(secret.Data["username"]),
			Password: string(secret.Data["password"]),
		}
	case "proxmox":
		creds.Proxmox = &addons.ProxmoxCredentials{
			Endpoint: string(secret.Data["endpoint"]),
			Username: string(secret.Data["username"]),
			Password: string(secret.Data["password"]),
		}
	}

	return creds, nil
}

func boolPtr(b bool) *bool {
	return &b
}

func (r *ClusterBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&butlerv1alpha1.ClusterBootstrap{}).
		Owns(&butlerv1alpha1.MachineRequest{}).
		Complete(r)
}
