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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
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
	ClusterName                    string
	ControlPlaneVIP                string
	ControlPlaneIPs                []string
	WorkerIPs                      []string
	PodCIDR                        string
	ServiceCIDR                    string
	TalosVersion                   string
	InstallDisk                    string
	ConfigPatches                  []ConfigPatch
	AllowSchedulingOnControlPlanes bool // For single-node topology
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
	InstallMetalLB(ctx context.Context, kubeconfig []byte, addressPool string, topology string) error
	InstallTraefik(ctx context.Context, kubeconfig []byte, version string) error
	InstallGatewayAPI(ctx context.Context, kubeconfig []byte, version string) error // NEW: Gateway API CRDs
	// InstallStewardCRDs(ctx context.Context, kubeconfig []byte, version string) error // NEW: Steward CRDs (optional)
	InstallSteward(ctx context.Context, kubeconfig []byte, version string) error // NEW: Replaces InstallKamaji
	InstallFlux(ctx context.Context, kubeconfig []byte) error
	InstallButler(ctx context.Context, kubeconfig []byte) error
	InstallButlerCRDs(ctx context.Context, kubeconfig []byte, version string) error
	InstallInitialProviderConfig(ctx context.Context, kubeconfig []byte, providerType string, creds *addons.ProviderCredentials) error
	InstallCAPI(ctx context.Context, kubeconfig []byte, version string, mgmtProvider string, additionalProviders []butlerv1alpha1.CAPIInfraProviderSpec, creds *addons.ProviderCredentials) error
	InstallButlerController(ctx context.Context, kubeconfig []byte, image string) error
	InstallButlerAddons(ctx context.Context, kubeconfig []byte, version string) error
	InstallConsole(ctx context.Context, kubeconfig []byte, spec *butlerv1alpha1.ConsoleAddonSpec, clusterName string) (string, error)
}

// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=clusterbootstraps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=clusterbootstraps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=clusterbootstraps/finalizers,verbs=update
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=machinerequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=providerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=butlerconfigs,verbs=get;list;watch;create;update;patch
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

	// Log topology information
	isSingleNode := cb.IsSingleNode()
	expectedCount := cb.GetExpectedMachineCount()
	logger.Info("Reconciling ProvisioningMachines phase",
		"topology", cb.Spec.Cluster.Topology,
		"singleNode", isSingleNode,
		"expectedMachines", expectedCount)

	clusterName := cb.Spec.Cluster.Name

	// Create control plane MachineRequests
	// Use GetControlPlaneReplicas() which returns 1 for single-node
	cpReplicas := cb.GetControlPlaneReplicas()
	for i := int32(0); i < cpReplicas; i++ {
		mrName := fmt.Sprintf("%s-cp-%d", clusterName, i)
		if err := r.ensureMachineRequest(ctx, cb, mrName, "control-plane", cb.Spec.Cluster.ControlPlane); err != nil {
			logger.Error(err, "Failed to ensure control plane MachineRequest", "name", mrName)
			return ctrl.Result{}, err
		}
	}

	// Create worker MachineRequests only if NOT single-node topology
	if !isSingleNode && cb.Spec.Cluster.Workers != nil {
		for i := int32(0); i < cb.Spec.Cluster.Workers.Replicas; i++ {
			mrName := fmt.Sprintf("%s-worker-%d", clusterName, i)
			if err := r.ensureMachineRequest(ctx, cb, mrName, "worker", *cb.Spec.Cluster.Workers); err != nil {
				logger.Error(err, "Failed to ensure worker MachineRequest", "name", mrName)
				return ctrl.Result{}, err
			}
		}
	} else if isSingleNode {
		logger.Info("Single-node topology: skipping worker MachineRequest creation")
	}

	// Update machine statuses
	if err := r.updateMachineStatuses(ctx, cb); err != nil {
		logger.Error(err, "Failed to update machine statuses")
		return ctrl.Result{}, err
	}

	// Check if all machines are ready using the CRD helper method
	if cb.AllMachinesRunning() {
		logger.Info("All machines are running, transitioning to ConfiguringTalos",
			"machineCount", len(cb.Status.Machines))
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

	logger.Info("Waiting for machines to be ready",
		"current", len(cb.Status.Machines),
		"expected", expectedCount)
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
	logger.Info("Reconciling ConfiguringTalos phase",
		"topology", cb.Spec.Cluster.Topology,
		"singleNode", cb.IsSingleNode())

	// Check if we need to generate configs
	if cb.Status.TalosConfig == "" {
		cpIPs := r.getControlPlaneIPs(cb)
		workerIPs := r.getWorkerIPs(cb)

		opts := TalosConfigOptions{
			ClusterName:                    cb.Spec.Cluster.Name,
			ControlPlaneVIP:                cb.Spec.Network.VIP,
			ControlPlaneIPs:                cpIPs,
			WorkerIPs:                      workerIPs,
			PodCIDR:                        cb.Spec.Network.PodCIDR,
			ServiceCIDR:                    cb.Spec.Network.ServiceCIDR,
			TalosVersion:                   cb.Spec.Talos.Version,
			InstallDisk:                    cb.Spec.Talos.InstallDisk,
			AllowSchedulingOnControlPlanes: cb.IsSingleNode(), // Enable for single-node
		}

		if cb.IsSingleNode() {
			logger.Info("Single-node mode: enabling workload scheduling on control planes")
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

// Part 2 of clusterbootstrap_controller.go - continues from part 1

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

	// 3. cert-manager - needed by Traefik and Steward webhooks
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

	// 4. Longhorn storage - use topology-aware replica count
	if addons.Storage != nil && addons.Storage.Type == "longhorn" {
		if !r.isAddonInstalled(cb, "longhorn") {
			logger.Info("Installing Longhorn")
			version := addons.Storage.Version

			// Use GetStorageReplicaCount() for topology-aware replica count
			// Returns 1 for single-node, 3 for HA (default)
			replicas := cb.GetStorageReplicaCount()

			logger.Info("Installing Longhorn with replica count",
				"replicas", replicas,
				"singleNode", cb.IsSingleNode())

			if err := r.AddonInstaller.InstallLonghorn(ctx, kubeconfig, version, replicas); err != nil {
				logger.Error(err, "Failed to install Longhorn")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "longhorn")
			logger.Info("Longhorn installed successfully", "replicaCount", replicas)
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Longhorn install", "error", err)
			}
		}
	}

	// 5. MetalLB
	if addons.LoadBalancer != nil && addons.LoadBalancer.Type == "metallb" && addons.LoadBalancer.AddressPool != "" {
		if !r.isAddonInstalled(cb, "metallb") {
			logger.Info("Installing MetalLB")

			if err := r.AddonInstaller.InstallMetalLB(ctx, kubeconfig, addons.LoadBalancer.AddressPool, string(cb.Spec.Cluster.Topology)); err != nil {
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

	// 7a. Gateway API CRDs - required for Steward TLSRoute support
	// Install before Steward so the CRDs exist when Steward starts watching them
	if !r.isAddonInstalled(cb, "gateway-api") {
		logger.Info("Installing Gateway API CRDs")
		if err := r.AddonInstaller.InstallGatewayAPI(ctx, kubeconfig, "v1.2.0"); err != nil {
			logger.Error(err, "Failed to install Gateway API CRDs")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		r.setAddonInstalled(cb, "gateway-api")
		logger.Info("Gateway API CRDs installed successfully")
		if err := r.Status().Update(ctx, cb); err != nil {
			logger.Info("Failed to update status after Gateway API install", "error", err)
		}
	}

	// 7b. Steward CRDs - install before controller to ensure CRDs exist
	// if addons.ControlPlaneProvider == nil || isAddonEnabled(addons.ControlPlaneProvider.Enabled) {
	// 	if !r.isAddonInstalled(cb, "steward-crds") {
	// 		logger.Info("Installing Steward CRDs")
	// 		version := ""
	// 		if addons.ControlPlaneProvider != nil && addons.ControlPlaneProvider.Version != "" {
	// 			version = addons.ControlPlaneProvider.Version
	// 		}
	//
	// 		if err := r.AddonInstaller.InstallStewardCRDs(ctx, kubeconfig, version); err != nil {
	// 			logger.Error(err, "Failed to install Steward CRDs")
	// 			return ctrl.Result{RequeueAfter: requeueShort}, nil
	// 		}
	//
	// 		r.setAddonInstalled(cb, "steward-crds")
	// 		logger.Info("Steward CRDs installed successfully")
	// 		if err := r.Status().Update(ctx, cb); err != nil {
	// 			logger.Info("Failed to update status after Steward CRDs install", "error", err)
	// 		}
	// 	}
	// }

	// 7c. Steward - hosted control planes (replaces Kamaji)
	if addons.ControlPlaneProvider == nil || isAddonEnabled(addons.ControlPlaneProvider.Enabled) {
		if !r.isAddonInstalled(cb, "steward") {
			logger.Info("Installing Steward")
			version := ""
			if addons.ControlPlaneProvider != nil && addons.ControlPlaneProvider.Version != "" {
				version = addons.ControlPlaneProvider.Version
			}

			if err := r.AddonInstaller.InstallSteward(ctx, kubeconfig, version); err != nil {
				logger.Error(err, "Failed to install Steward")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "steward")
			logger.Info("Steward installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Steward install", "error", err)
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

	// 9.5. Butler CRDs
	if addons.IsButlerControllerEnabled() {
		if !r.isAddonInstalled(cb, "butler-crds") {
			logger.Info("Installing Butler CRDs")

			if err := r.AddonInstaller.InstallButlerCRDs(ctx, kubeconfig, "0.1.0"); err != nil {
				logger.Error(err, "Failed to install Butler CRDs")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "butler-crds")
			logger.Info("Butler CRDs installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Butler CRDs install", "error", err)
			}
		}
	}
	// 9.6. Initial ProviderConfig - copies provider config from KIND to mgmt cluster
	if !r.isAddonInstalled(cb, "provider-config") {
		providerType := cb.Spec.Provider
		creds, err := r.extractProviderCredentials(ctx, cb)
		if err != nil {
			logger.Error(err, "Failed to extract provider credentials")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}

		if err := r.AddonInstaller.InstallInitialProviderConfig(ctx, kubeconfig, providerType, creds); err != nil {
			logger.Error(err, "Failed to create initial ProviderConfig")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
		r.setAddonInstalled(cb, "provider-config")
	}

	// 9.7. Butler Addons (addon definitions catalog)
	if addons.IsButlerControllerEnabled() {
		if !r.isAddonInstalled(cb, "butler-addons") {
			logger.Info("Installing Butler addon definitions")
			if err := r.AddonInstaller.InstallButlerAddons(ctx, kubeconfig, "0.1.0"); err != nil {
				logger.Error(err, "Failed to install Butler addon definitions")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}
			r.setAddonInstalled(cb, "butler-addons")
			logger.Info("Butler addon definitions installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Butler addons install", "error", err)
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

	// 11.5. Write ControlPlaneExposure config to ButlerConfig singleton
	// This writes the platform-level exposure mode (LoadBalancer/Ingress/Gateway)
	// which all TenantClusters will inherit
	if !r.isAddonInstalled(cb, "butler-config-exposure") {
		if cb.Spec.ControlPlaneExposure != nil {
			logger.Info("Writing ControlPlaneExposure to ButlerConfig",
				"mode", cb.Spec.ControlPlaneExposure.Mode)

			if err := r.reconcileButlerConfig(ctx, cb, kubeconfig); err != nil {
				logger.Error(err, "Failed to write ButlerConfig")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "butler-config-exposure")
			logger.Info("ButlerConfig exposure settings written successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after ButlerConfig write", "error", err)
			}
		} else {
			// No exposure config specified, skip but mark as done
			r.setAddonInstalled(cb, "butler-config-exposure")
		}
	}

	// 12. Butler Console
	if addons.IsConsoleEnabled() {
		if !r.isAddonInstalled(cb, "butler-console") {
			logger.Info("Installing Butler Console")

			consoleURL, err := r.AddonInstaller.InstallConsole(ctx, kubeconfig, addons.Console, cb.Spec.Cluster.Name)
			if err != nil {
				logger.Error(err, "Failed to install Butler Console")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			// Store console URL in status for orchestrator to retrieve
			cb.Status.ConsoleURL = consoleURL

			r.setAddonInstalled(cb, "butler-console")
			logger.Info("Butler Console installed successfully", "url", consoleURL)
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Info("Failed to update status after Console install", "error", err)
			}
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
		// Endpoint, port, insecure come from ProviderConfig spec
		// Username, password come from Secret
		endpoint := providerConfig.Spec.Nutanix.Endpoint
		port := "9440"
		if providerConfig.Spec.Nutanix.Port > 0 {
			port = fmt.Sprintf("%d", providerConfig.Spec.Nutanix.Port)
		}

		creds.Nutanix = &addons.NutanixCredentials{
			Endpoint: endpoint,
			Username: string(secret.Data["username"]),
			Password: string(secret.Data["password"]),
			Port:     port,
			Insecure: providerConfig.Spec.Nutanix.Insecure,
		}
		logger.Info("Retrieved Nutanix credentials", "endpoint", creds.Nutanix.Endpoint, "username", creds.Nutanix.Username)
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

// extractProviderCredentials fetches credentials from the referenced ProviderConfig
func (r *ClusterBootstrapReconciler) extractProviderCredentials(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (*addons.ProviderCredentials, error) {
	logger := log.FromContext(ctx)
	creds := &addons.ProviderCredentials{}

	// Get the ProviderConfig from the local (KIND) cluster
	providerConfig := &butlerv1alpha1.ProviderConfig{}
	providerConfigKey := types.NamespacedName{
		Name:      cb.Spec.ProviderRef.Name,
		Namespace: cb.Spec.ProviderRef.Namespace,
	}
	if providerConfigKey.Namespace == "" {
		providerConfigKey.Namespace = cb.Namespace
	}

	if err := r.Get(ctx, providerConfigKey, providerConfig); err != nil {
		return nil, fmt.Errorf("failed to get ProviderConfig %s: %w", providerConfigKey, err)
	}

	// Get the credentials secret
	secretKey := types.NamespacedName{
		Name:      providerConfig.Spec.CredentialsRef.Name,
		Namespace: providerConfig.Spec.CredentialsRef.Namespace,
	}
	if secretKey.Namespace == "" {
		secretKey.Namespace = providerConfig.Namespace
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, fmt.Errorf("failed to get credentials secret %s: %w", secretKey, err)
	}

	switch cb.Spec.Provider {
	case "nutanix":
		if providerConfig.Spec.Nutanix == nil {
			return nil, fmt.Errorf("ProviderConfig %s has no nutanix configuration", providerConfigKey)
		}
		port := providerConfig.Spec.Nutanix.Port
		if port == 0 {
			port = 9440
		}
		creds.Nutanix = &addons.NutanixCredentials{
			Endpoint:    providerConfig.Spec.Nutanix.Endpoint,
			Username:    string(secret.Data["username"]),
			Password:    string(secret.Data["password"]),
			Port:        fmt.Sprintf("%d", port),
			Insecure:    providerConfig.Spec.Nutanix.Insecure,
			ClusterUUID: providerConfig.Spec.Nutanix.ClusterUUID,
			SubnetUUID:  providerConfig.Spec.Nutanix.SubnetUUID,
			ImageUUID:   providerConfig.Spec.Nutanix.ImageUUID,
		}
		logger.Info("Extracted Nutanix credentials",
			"endpoint", creds.Nutanix.Endpoint,
			"clusterUUID", creds.Nutanix.ClusterUUID,
			"subnetUUID", creds.Nutanix.SubnetUUID)

	case "harvester":
		if providerConfig.Spec.Harvester == nil {
			return nil, fmt.Errorf("ProviderConfig %s has no harvester configuration", providerConfigKey)
		}
		creds.Harvester = &addons.HarvesterCredentials{
			Kubeconfig:       secret.Data["kubeconfig"],
			Namespace:        providerConfig.Spec.Harvester.Namespace,
			NetworkName:      providerConfig.Spec.Harvester.NetworkName,
			ImageName:        providerConfig.Spec.Harvester.ImageName,
			StorageClassName: providerConfig.Spec.Harvester.StorageClassName,
		}
		logger.Info("Extracted Harvester credentials",
			"namespace", creds.Harvester.Namespace,
			"networkName", creds.Harvester.NetworkName)

	default:
		logger.Info("Unknown or unsupported provider type", "provider", cb.Spec.Provider)
	}

	return creds, nil
}

func boolPtr(b bool) *bool {
	return &b
}

// reconcileButlerConfig writes ControlPlaneExposure config from ClusterBootstrap to ButlerConfig singleton
// on the target management cluster
func (r *ClusterBootstrapReconciler) reconcileButlerConfig(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap, kubeconfig []byte) error {
	logger := log.FromContext(ctx)

	// Create a client to the target cluster using the kubeconfig
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create REST config from kubeconfig: %w", err)
	}

	targetClient, err := client.New(restConfig, client.Options{Scheme: r.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client for target cluster: %w", err)
	}

	// Get or create ButlerConfig singleton
	bc := &butlerv1alpha1.ButlerConfig{}
	err = targetClient.Get(ctx, client.ObjectKey{Name: "butler"}, bc)
	if err != nil {
		if errors.IsNotFound(err) {
			// Create new ButlerConfig
			bc = &butlerv1alpha1.ButlerConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "butler",
				},
				Spec: butlerv1alpha1.ButlerConfigSpec{},
			}
		} else {
			return fmt.Errorf("failed to get ButlerConfig: %w", err)
		}
	}

	// Copy ControlPlaneExposure from ClusterBootstrap to ButlerConfig
	if cb.Spec.ControlPlaneExposure != nil {
		bc.Spec.ControlPlaneExposure = cb.Spec.ControlPlaneExposure.DeepCopy()
		logger.Info("Setting ControlPlaneExposure on ButlerConfig",
			"mode", bc.Spec.ControlPlaneExposure.Mode,
			"hostname", bc.Spec.ControlPlaneExposure.Hostname)
	}

	// Create or update
	if bc.ResourceVersion == "" {
		// Create
		if err := targetClient.Create(ctx, bc); err != nil {
			return fmt.Errorf("failed to create ButlerConfig: %w", err)
		}
		logger.Info("Created ButlerConfig with ControlPlaneExposure settings")
	} else {
		// Update
		if err := targetClient.Update(ctx, bc); err != nil {
			return fmt.Errorf("failed to update ButlerConfig: %w", err)
		}
		logger.Info("Updated ButlerConfig with ControlPlaneExposure settings")
	}

	// Update ButlerConfig status
	bc.Status.ControlPlaneExposureMode = cb.GetControlPlaneExposureMode()
	bc.Status.TCPProxyRequired = cb.IsTCPProxyRequired()

	if err := targetClient.Status().Update(ctx, bc); err != nil {
		logger.Info("Failed to update ButlerConfig status", "error", err)
		// Don't fail on status update error
	}

	return nil
}

func (r *ClusterBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&butlerv1alpha1.ClusterBootstrap{}).
		Owns(&butlerv1alpha1.MachineRequest{}).
		Complete(r)
}
