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
	"net"
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
	Platform                       string // Cloud platform for Talos metadata discovery (gcp, aws, azure). Empty for on-prem.
	ConfigPatches                  []ConfigPatch
	ControlPlaneConfigPatches      []ConfigPatch // Patches applied only to control plane configs
	AllowSchedulingOnControlPlanes bool          // For single-node topology
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
	InstallCilium(ctx context.Context, kubeconfig []byte, version string, hubbleEnabled bool, gatewayAPIEnabled bool) error
	InstallCertManager(ctx context.Context, kubeconfig []byte, version string) error
	InstallLonghorn(ctx context.Context, kubeconfig []byte, version string, replicaCount int32) error
	InstallMetalLB(ctx context.Context, kubeconfig []byte, addressPool string, topology string) error
	InstallCloudControllerManager(ctx context.Context, kubeconfig []byte, provider string) error
	InstallTraefik(ctx context.Context, kubeconfig []byte, version string) error
	InstallGatewayAPI(ctx context.Context, kubeconfig []byte, version string) error
	InstallSteward(ctx context.Context, kubeconfig []byte, version string) error
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
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=imagesyncs,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=loadbalancerrequests,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=loadbalancerrequests/status,verbs=get
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

	// For cloud HA topologies, ensure a LoadBalancerRequest exists so the
	// provider controller can provision an LB before Talos configs are generated.
	if cb.IsCloudProvider() && !isSingleNode {
		if err := r.ensureLoadBalancerRequest(ctx, cb); err != nil {
			logger.Error(err, "Failed to ensure LoadBalancerRequest")
			return ctrl.Result{}, err
		}
	}

	// Resolve OS image via ImageSync before creating MachineRequests.
	// This ensures the image is available on the provider before VMs are created.
	providerImageRef, err := r.reconcileImageSync(ctx, cb)
	if err != nil {
		logger.Info("Image sync not yet ready, waiting", "error", err)
		if statusErr := r.Status().Update(ctx, cb); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueMedium}, nil
	}

	if providerImageRef != "" {
		logger.Info("Image sync resolved provider image", "providerImageRef", providerImageRef)
	}

	clusterName := cb.Spec.Cluster.Name

	// Create control plane MachineRequests
	// Use GetControlPlaneReplicas() which returns 1 for single-node
	cpReplicas := cb.GetControlPlaneReplicas()
	for i := int32(0); i < cpReplicas; i++ {
		mrName := fmt.Sprintf("%s-cp-%d", clusterName, i)
		if err := r.ensureMachineRequest(ctx, cb, mrName, "control-plane", cb.Spec.Cluster.ControlPlane, providerImageRef); err != nil {
			logger.Error(err, "Failed to ensure control plane MachineRequest", "name", mrName)
			return ctrl.Result{}, err
		}
	}

	// Create worker MachineRequests only if NOT single-node topology
	if !isSingleNode && cb.Spec.Cluster.Workers != nil {
		for i := int32(0); i < cb.Spec.Cluster.Workers.Replicas; i++ {
			mrName := fmt.Sprintf("%s-worker-%d", clusterName, i)
			if err := r.ensureMachineRequest(ctx, cb, mrName, "worker", *cb.Spec.Cluster.Workers, providerImageRef); err != nil {
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

	// Update LB targets with running control plane instances
	if cb.IsCloudProvider() && !isSingleNode {
		if err := r.updateLoadBalancerTargets(ctx, cb); err != nil {
			logger.Error(err, "Failed to update LB targets")
		}
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

func (r *ClusterBootstrapReconciler) ensureMachineRequest(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap, name string, role string, pool butlerv1alpha1.ClusterBootstrapNodePool, imageOverride string) error {
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
			Image:       imageOverride,
		},
	}

	if err := r.Create(ctx, mr); err != nil {
		return err
	}

	logger.Info("Created MachineRequest", "name", name, "role", role, "image", imageOverride)
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

		endpoint := r.resolveControlPlaneEndpoint(ctx, cb)
		if endpoint == "" {
			// For cloud HA, check if the LBR has failed so we don't requeue forever.
			if cb.IsCloudProvider() && !cb.IsSingleNode() {
				lbr := &butlerv1alpha1.LoadBalancerRequest{}
				if err := r.Get(ctx, client.ObjectKey{Name: lbrName(cb), Namespace: cb.Namespace}, lbr); err == nil && lbr.IsFailed() {
					msg := lbr.Status.FailureMessage
					if msg == "" {
						msg = "LoadBalancerRequest failed with no message"
					}
					r.setFailure(cb, "LoadBalancerFailed", fmt.Sprintf("Control plane LB %s failed: %s", lbrName(cb), msg))
					r.Status().Update(ctx, cb)
					return ctrl.Result{}, nil
				}
			}
			logger.Info("Control plane endpoint not yet available (no VIP and no CP IPs discovered), requeueing")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}

		opts := TalosConfigOptions{
			ClusterName:                    cb.Spec.Cluster.Name,
			ControlPlaneVIP:                endpoint,
			ControlPlaneIPs:                cpIPs,
			WorkerIPs:                      workerIPs,
			PodCIDR:                        cb.Spec.Network.PodCIDR,
			ServiceCIDR:                    cb.Spec.Network.ServiceCIDR,
			TalosVersion:                   cb.Spec.Talos.Version,
			InstallDisk:                    cb.Spec.Talos.InstallDisk,
			Platform:                       r.getTalosPlatform(cb.Spec.Provider),
			AllowSchedulingOnControlPlanes: cb.IsSingleNode() || cb.Spec.Cluster.Workers == nil || cb.Spec.Cluster.Workers.Replicas == 0,
		}

		if cb.IsSingleNode() {
			logger.Info("Single-node mode: enabling workload scheduling on control planes")
		} else if cb.Spec.Cluster.Workers == nil || cb.Spec.Cluster.Workers.Replicas == 0 {
			logger.Info("No workers configured: enabling workload scheduling on control planes")
		}

		// Convert config patches
		for _, p := range cb.Spec.Talos.ConfigPatches {
			opts.ConfigPatches = append(opts.ConfigPatches, ConfigPatch{
				Op:    p.Op,
				Path:  p.Path,
				Value: p.Value,
			})
		}

		// For cloud HA, add the LB IP to the loopback interface on CP nodes.
		// GCP (and other cloud) passthrough NLBs deliver packets with the
		// destination IP set to the LB's IP. The kernel must recognize this
		// IP as local to accept the traffic. Adding it to the loopback
		// interface achieves this without conflicting with the primary NIC.
		if cb.IsCloudProvider() && !cb.IsSingleNode() && endpoint != "" {
			if net.ParseIP(endpoint) == nil {
				logger.Info("Skipping loopback patch: endpoint is a DNS name, not an IP", "endpoint", endpoint)
			} else {
				// NOTE: This uses "add" which will replace any existing /machine/network/interfaces
				// configuration. This is acceptable for cloud bootstrap because Talos auto-discovers
				// the primary interface via the cloud platform metadata service (IMDS) rather than
				// static config. If custom interface patches are needed, they should be applied
				// after bootstrap via talosctl.
				logger.Info("Adding cloud LB IP to CP loopback interface", "ip", endpoint)
				opts.ControlPlaneConfigPatches = append(opts.ControlPlaneConfigPatches, ConfigPatch{
					Op:    "add",
					Path:  "/machine/network/interfaces",
					Value: fmt.Sprintf(`[{"interface":"lo","addresses":["%s/32"]}]`, endpoint),
				})
			}
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
				strings.Contains(err.Error(), "i/o timeout") ||
				strings.Contains(err.Error(), "bootstrap is not available yet") {
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
		cb.Status.ControlPlaneEndpoint = fmt.Sprintf("https://%s:6443", r.resolveControlPlaneEndpoint(ctx, cb))
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

	// 1. kube-vip - required for VIP to work on on-prem providers.
	// Cloud providers (gcp, aws, azure) skip kube-vip because cloud networks
	// do not support gratuitous ARP. Cloud HA uses a cloud load balancer instead.
	if !r.isAddonInstalled(cb, "kube-vip") {
		if cb.Spec.Network.VIP != "" && !cb.IsCloudProvider() {
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
			logger.Info("kube-vip installed successfully")
		} else if cb.IsCloudProvider() {
			logger.Info("Skipping kube-vip (cloud provider uses cloud load balancer)")
		} else {
			logger.Info("Skipping kube-vip (no VIP configured)")
		}

		r.setAddonInstalled(cb, "kube-vip")
		if err := r.Status().Update(ctx, cb); err != nil {
			logger.Error(err, "Failed to update status after kube-vip step")
		}
	}

	// 2. Cilium CNI
	if addons.CNI != nil && addons.CNI.Type == "cilium" {
		if !r.isAddonInstalled(cb, "cilium") {
			logger.Info("Installing Cilium")
			version := addons.CNI.Version
			hubble := addons.CNI.HubbleEnabled
			gatewayAPI := cb.Spec.ControlPlaneExposure != nil &&
				cb.Spec.ControlPlaneExposure.Mode == butlerv1alpha1.ControlPlaneExposureModeGateway

			if err := r.AddonInstaller.InstallCilium(ctx, kubeconfig, version, hubble, gatewayAPI); err != nil {
				logger.Error(err, "Failed to install Cilium")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "cilium")
			logger.Info("Cilium installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Error(err, "Failed to update status after Cilium install")
			}
		}
	}

	// 2.5 Cloud Controller Manager - skipped for management cluster bootstrap.
	// Management clusters don't need CCM. Setting --cloud-provider=external
	// on kubelet adds an "uninitialized" taint that blocks all pod scheduling
	// until CCM runs, creating a deadlock. Tenant clusters on cloud providers
	// get CCM installed by butler-controller via CAPI machine templates.
	if cb.IsCloudProvider() {
		r.ensureAddonsMap(cb)
		cb.Status.AddonsInstalled["cloud-controller-manager"] = true
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
				logger.Error(err, "Failed to update status after cert-manager install")
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
				logger.Error(err, "Failed to update status after Longhorn install")
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
				logger.Error(err, "Failed to update status after MetalLB install")
			}
		}
	}

	// 6. Traefik ingress - after MetalLB so it can get LoadBalancer IP.
	// Cloud providers skip Traefik because there is no MetalLB or CCM to
	// allocate LoadBalancer IPs on the management cluster. Traefik's LB
	// Service stays <pending> and Helm --wait times out.
	if !r.isAddonInstalled(cb, "traefik") {
		if cb.IsCloudProvider() {
			logger.Info("Skipping Traefik (cloud provider has no MetalLB/CCM for LoadBalancer)")
		} else if addons.Ingress == nil || isAddonEnabled(addons.Ingress.Enabled) {
			logger.Info("Installing Traefik")
			version := ""
			if addons.Ingress != nil && addons.Ingress.Version != "" {
				version = addons.Ingress.Version
			}

			if err := r.AddonInstaller.InstallTraefik(ctx, kubeconfig, version); err != nil {
				logger.Error(err, "Failed to install Traefik")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			logger.Info("Traefik installed successfully")
		}

		r.setAddonInstalled(cb, "traefik")
		if err := r.Status().Update(ctx, cb); err != nil {
			logger.Error(err, "Failed to update status after Traefik step")
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
			logger.Error(err, "Failed to update status after Gateway API install")
		}
	}

	// 7b. Steward - hosted control planes (replaces Kamaji)
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
				logger.Error(err, "Failed to update status after Steward install")
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
				logger.Error(err, "Failed to update status after CAPI install")
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
				logger.Error(err, "Failed to update status after Flux install")
			}
		}
	}

	// 9.5. Butler CRDs
	if addons.IsButlerControllerEnabled() {
		if !r.isAddonInstalled(cb, "butler-crds") {
			logger.Info("Installing Butler CRDs")

			if err := r.AddonInstaller.InstallButlerCRDs(ctx, kubeconfig, ""); err != nil {
				logger.Error(err, "Failed to install Butler CRDs")
				return ctrl.Result{RequeueAfter: requeueShort}, nil
			}

			r.setAddonInstalled(cb, "butler-crds")
			logger.Info("Butler CRDs installed successfully")
			if err := r.Status().Update(ctx, cb); err != nil {
				logger.Error(err, "Failed to update status after Butler CRDs install")
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
				logger.Error(err, "Failed to update status after Butler addons install")
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
				logger.Error(err, "Failed to update status after Butler Controller install")
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
			logger.Error(err, "Failed to update status after Butler install")
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
				logger.Error(err, "Failed to update status after ButlerConfig write")
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
				logger.Error(err, "Failed to update status after Console install")
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

// resolveControlPlaneEndpoint returns the control plane API server endpoint.
// For on-prem providers with kube-vip, this is the VIP from the spec. For cloud
// HA providers, this is the LoadBalancerRequest endpoint (blocks until Ready).
// For single-node cloud, this falls back to the first control plane node's IP.
func (r *ClusterBootstrapReconciler) resolveControlPlaneEndpoint(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) string {
	if cb.Spec.Network.VIP != "" {
		return cb.Spec.Network.VIP
	}

	// For cloud HA, require the LoadBalancerRequest endpoint — do NOT fall
	// back to a node IP because Talos configs are generated once and the
	// endpoint is permanently baked in.
	if cb.IsCloudProvider() && !cb.IsSingleNode() {
		lbr := &butlerv1alpha1.LoadBalancerRequest{}
		err := r.Get(ctx, client.ObjectKey{Name: lbrName(cb), Namespace: cb.Namespace}, lbr)
		if err == nil && lbr.IsReady() && lbr.Status.Endpoint != "" {
			return lbr.Status.Endpoint
		}
		// LBR not ready yet — return empty to trigger requeue in caller
		return ""
	}

	cpIPs := r.getControlPlaneIPs(cb)
	if len(cpIPs) > 0 {
		return cpIPs[0]
	}
	return ""
}

// getTalosPlatform maps the bootstrap provider to a Talos platform identifier.
// Cloud platforms need this so Talos can query instance metadata services for
// networking configuration and cloud-init data.
func (r *ClusterBootstrapReconciler) getTalosPlatform(provider string) string {
	switch provider {
	case "gcp":
		return "gcp"
	case "aws":
		return "aws"
	case "azure":
		return "azure"
	default:
		return "" // on-prem providers use the default metal platform
	}
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
	case "gcp":
		return "ens4"
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
	case "gcp":
		creds.GCP = &addons.GCPCredentials{
			ServiceAccountKey: string(secret.Data["serviceAccountKey"]),
			ProjectID:         providerConfig.Spec.GCP.ProjectID,
			Region:            providerConfig.Spec.GCP.Region,
		}
		if providerConfig.Spec.GCP.Network != "" {
			creds.GCP.Network = providerConfig.Spec.GCP.Network
		}
		logger.Info("Retrieved GCP credentials", "projectID", creds.GCP.ProjectID, "region", creds.GCP.Region)
	case "aws":
		creds.AWS = &addons.AWSCredentials{
			AccessKeyID:    string(secret.Data["accessKeyID"]),
			SecretAccessKey: string(secret.Data["secretAccessKey"]),
			Region:          providerConfig.Spec.AWS.Region,
		}
		if providerConfig.Spec.AWS.VPCID != "" {
			creds.AWS.VPCID = providerConfig.Spec.AWS.VPCID
		}
		if len(providerConfig.Spec.AWS.SubnetIDs) > 0 {
			creds.AWS.SubnetID = providerConfig.Spec.AWS.SubnetIDs[0]
		}
		if len(providerConfig.Spec.AWS.SecurityGroupIDs) > 0 {
			creds.AWS.SecurityGroupID = providerConfig.Spec.AWS.SecurityGroupIDs[0]
		}
		logger.Info("Retrieved AWS credentials", "region", creds.AWS.Region)
	case "azure":
		creds.Azure = &addons.AzureCredentials{
			ClientID:       string(secret.Data["clientID"]),
			ClientSecret:   string(secret.Data["clientSecret"]),
			TenantID:       string(secret.Data["tenantID"]),
			SubscriptionID: string(secret.Data["subscriptionID"]),
			ResourceGroup:  providerConfig.Spec.Azure.ResourceGroup,
			Location:       providerConfig.Spec.Azure.Location,
			VNetName:       providerConfig.Spec.Azure.VNetName,
			SubnetName:     providerConfig.Spec.Azure.SubnetName,
		}
		logger.Info("Retrieved Azure credentials", "subscriptionID", creds.Azure.SubscriptionID, "resourceGroup", creds.Azure.ResourceGroup)
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

	case "gcp":
		if providerConfig.Spec.GCP == nil {
			return nil, fmt.Errorf("ProviderConfig %s has no gcp configuration", providerConfigKey)
		}
		creds.GCP = &addons.GCPCredentials{
			ServiceAccountKey: string(secret.Data["serviceAccountKey"]),
			ProjectID:         providerConfig.Spec.GCP.ProjectID,
			Region:            providerConfig.Spec.GCP.Region,
			Network:           providerConfig.Spec.GCP.Network,
			Subnetwork:        providerConfig.Spec.GCP.Subnetwork,
		}
		logger.Info("Extracted GCP credentials",
			"projectID", creds.GCP.ProjectID,
			"region", creds.GCP.Region)

	case "aws":
		if providerConfig.Spec.AWS == nil {
			return nil, fmt.Errorf("ProviderConfig %s has no aws configuration", providerConfigKey)
		}
		creds.AWS = &addons.AWSCredentials{
			AccessKeyID:    string(secret.Data["accessKeyID"]),
			SecretAccessKey: string(secret.Data["secretAccessKey"]),
			Region:          providerConfig.Spec.AWS.Region,
			VPCID:           providerConfig.Spec.AWS.VPCID,
		}
		if len(providerConfig.Spec.AWS.SubnetIDs) > 0 {
			creds.AWS.SubnetID = providerConfig.Spec.AWS.SubnetIDs[0]
		}
		if len(providerConfig.Spec.AWS.SecurityGroupIDs) > 0 {
			creds.AWS.SecurityGroupID = providerConfig.Spec.AWS.SecurityGroupIDs[0]
		}
		logger.Info("Extracted AWS credentials",
			"region", creds.AWS.Region,
			"vpcID", creds.AWS.VPCID)

	case "azure":
		if providerConfig.Spec.Azure == nil {
			return nil, fmt.Errorf("ProviderConfig %s has no azure configuration", providerConfigKey)
		}
		creds.Azure = &addons.AzureCredentials{
			ClientID:       string(secret.Data["clientID"]),
			ClientSecret:   string(secret.Data["clientSecret"]),
			TenantID:       string(secret.Data["tenantID"]),
			SubscriptionID: string(secret.Data["subscriptionID"]),
			ResourceGroup:  providerConfig.Spec.Azure.ResourceGroup,
			Location:       providerConfig.Spec.Azure.Location,
			VNetName:       providerConfig.Spec.Azure.VNetName,
			SubnetName:     providerConfig.Spec.Azure.SubnetName,
		}
		logger.Info("Extracted Azure credentials",
			"subscriptionID", creds.Azure.SubscriptionID,
			"resourceGroup", creds.Azure.ResourceGroup)

	default:
		logger.Info("Unknown or unsupported provider type", "provider", cb.Spec.Provider)
	}

	return creds, nil
}

// lbrName returns the LoadBalancerRequest name for a ClusterBootstrap's control plane LB.
func lbrName(cb *butlerv1alpha1.ClusterBootstrap) string {
	return cb.Spec.Cluster.Name + "-cp-lb"
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
		logger.Error(err, "Failed to update ButlerConfig status")
		// Don't fail on status update error
	}

	return nil
}

// reconcileImageSync ensures the OS image is synced to the infrastructure provider
// via the Butler Image Factory before MachineRequests are created.
//
// It resolves the schematicID from the ClusterBootstrap spec (talos.schematic) or
// the ButlerConfig default, creates an ImageSync resource for deduplication, and
// returns the provider-specific image reference once the sync is complete.
//
// Returns:
//   - providerImageRef: the provider-specific image reference (e.g., "default/talos-v1.12.4-amd64").
//     Empty string if no sync is needed (no schematic configured, auto-sync disabled, or
//     no ButlerConfig/ImageFactory configured).
//   - error: non-nil triggers a requeue (image sync in progress, failed, or creation error).
func (r *ClusterBootstrapReconciler) reconcileImageSync(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) (string, error) {
	logger := log.FromContext(ctx)

	// Resolve schematicID: ClusterBootstrap talos.schematic takes precedence
	schematicID := cb.Spec.Talos.Schematic
	if schematicID == "" {
		// Fall back to ButlerConfig default
		bc := &butlerv1alpha1.ButlerConfig{}
		if err := r.Get(ctx, client.ObjectKey{Name: "butler"}, bc); err != nil {
			if errors.IsNotFound(err) {
				// No ButlerConfig — no image sync needed
				logger.V(1).Info("No ButlerConfig found, skipping image sync")
				return "", nil
			}
			return "", fmt.Errorf("failed to get ButlerConfig: %w", err)
		}

		if bc.Spec.ImageFactory != nil {
			schematicID = bc.Spec.ImageFactory.DefaultSchematicID
		}

		if schematicID == "" {
			// No schematic configured anywhere — skip image sync
			return "", nil
		}

		// Image Factory must be configured if a schematicID is set
		if !bc.IsImageFactoryConfigured() {
			return "", fmt.Errorf("schematicID %q is set but Image Factory is not configured in ButlerConfig", schematicID)
		}

		// Check if auto-sync is enabled
		if !bc.IsAutoSyncEnabled() {
			logger.V(1).Info("Image auto-sync disabled in ButlerConfig, skipping ImageSync creation")
			return "", nil
		}
	} else {
		// SchematicID is set directly on the ClusterBootstrap — verify ButlerConfig
		bc := &butlerv1alpha1.ButlerConfig{}
		if err := r.Get(ctx, client.ObjectKey{Name: "butler"}, bc); err != nil {
			if errors.IsNotFound(err) {
				return "", fmt.Errorf("schematicID %q is set but no ButlerConfig exists", schematicID)
			}
			return "", fmt.Errorf("failed to get ButlerConfig: %w", err)
		}

		if !bc.IsImageFactoryConfigured() {
			return "", fmt.Errorf("schematicID %q is set but Image Factory is not configured in ButlerConfig", schematicID)
		}

		if !bc.IsAutoSyncEnabled() {
			logger.V(1).Info("Image auto-sync disabled in ButlerConfig, skipping ImageSync creation")
			return "", nil
		}
	}

	// Resolve image version, architecture, and platform
	version := cb.Spec.Talos.Version
	// Architecture defaults to amd64 — all current Butler providers target amd64.
	// When multi-arch support is added, this should read from the ClusterBootstrap spec.
	arch := "amd64"
	// Platform is the artifact name prefix in the factory URL.
	// Platform for ImageSync. On-prem uses "talos" (Butler Image Factory naming).
	// Cloud providers use the cloud platform name for cloud-specific image variants.
	platform := "talos"
	if cb.IsCloudProvider() {
		platform = cb.Spec.Provider
	}

	// Truncate schematicID to 63 chars for label value (Kubernetes label limit)
	labelSchematicID := schematicID
	if len(labelSchematicID) > 63 {
		labelSchematicID = labelSchematicID[:63]
	}

	// Build label selector for deduplication across ClusterBootstraps
	matchLabels := map[string]string{
		butlerv1alpha1.LabelSchematicID:    labelSchematicID,
		butlerv1alpha1.LabelImageVersion:   version,
		butlerv1alpha1.LabelProviderConfig: cb.Spec.ProviderRef.Name,
		butlerv1alpha1.LabelImageArch:      arch,
	}

	// List existing ImageSyncs matching labels in the bootstrap's namespace
	var existingList butlerv1alpha1.ImageSyncList
	if err := r.List(ctx, &existingList,
		client.InNamespace(cb.Namespace),
		client.MatchingLabels(matchLabels),
	); err != nil {
		return "", fmt.Errorf("failed to list ImageSyncs: %w", err)
	}

	if len(existingList.Items) > 0 {
		is := &existingList.Items[0]

		switch {
		case is.IsReady():
			logger.Info("ImageSync is ready",
				"name", is.Name,
				"providerImageRef", is.Status.ProviderImageRef)
			return is.Status.ProviderImageRef, nil

		case is.IsFailed():
			reason := is.Status.FailureReason
			if reason == "" {
				reason = "ImageSyncFailed"
			}
			message := is.Status.FailureMessage
			if message == "" {
				message = "Image sync failed"
			}
			r.setFailure(cb, reason, fmt.Sprintf("ImageSync %s failed: %s", is.Name, message))
			return "", fmt.Errorf("ImageSync %s failed: %s", is.Name, message)

		default:
			// In progress (Pending, Building, Downloading, Uploading)
			logger.Info("ImageSync in progress, requeueing",
				"name", is.Name,
				"phase", is.Status.Phase)
			return "", fmt.Errorf("ImageSync %s is in progress (phase: %s)", is.Name, is.Status.Phase)
		}
	}

	// No existing ImageSync found — create one

	// Build a short, DNS-safe name
	schematicPrefix := schematicID
	if len(schematicPrefix) > 8 {
		schematicPrefix = schematicPrefix[:8]
	}
	// Sanitize version for DNS label (replace dots with dashes, strip leading 'v')
	sanitizedVersion := strings.ReplaceAll(version, ".", "-")
	sanitizedVersion = strings.TrimPrefix(sanitizedVersion, "v")
	imageSyncName := fmt.Sprintf("%s-%s-%s", cb.Spec.Cluster.Name, schematicPrefix, sanitizedVersion)
	// Ensure name is DNS-safe (max 253 chars for object names, but keep it reasonable)
	if len(imageSyncName) > 63 {
		imageSyncName = imageSyncName[:63]
	}

	// Resolve provider config reference
	pcRef := butlerv1alpha1.ProviderReference{
		Name: cb.Spec.ProviderRef.Name,
	}
	if cb.Spec.ProviderRef.Namespace != "" && cb.Spec.ProviderRef.Namespace != cb.Namespace {
		pcRef.Namespace = cb.Spec.ProviderRef.Namespace
	}

	is := &butlerv1alpha1.ImageSync{
		ObjectMeta: metav1.ObjectMeta{
			Name:      imageSyncName,
			Namespace: cb.Namespace,
			Labels: map[string]string{
				butlerv1alpha1.LabelManagedBy:       "butler",
				butlerv1alpha1.LabelSchematicID:     labelSchematicID,
				butlerv1alpha1.LabelImageVersion:    version,
				butlerv1alpha1.LabelProviderConfig:  cb.Spec.ProviderRef.Name,
				butlerv1alpha1.LabelImageArch:       arch,
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
		Spec: butlerv1alpha1.ImageSyncSpec{
			FactoryRef: butlerv1alpha1.ImageFactoryRef{
				SchematicID: schematicID,
				Version:     version,
				Arch:        arch,
				Platform:    platform,
			},
			ProviderConfigRef: pcRef,
			Format:            "qcow2",
			TransferMode:      butlerv1alpha1.TransferModeDirect,
		},
	}

	logger.Info("Creating ImageSync",
		"name", is.Name,
		"schematicID", schematicID,
		"version", version,
		"provider", cb.Spec.ProviderRef.Name)

	if err := r.Create(ctx, is); err != nil {
		if errors.IsAlreadyExists(err) {
			// Race condition — another reconcile created it. Requeue to pick it up.
			return "", fmt.Errorf("ImageSync %s was just created, requeueing", imageSyncName)
		}
		return "", fmt.Errorf("failed to create ImageSync %s: %w", imageSyncName, err)
	}

	return "", fmt.Errorf("ImageSync %s created, waiting for completion", imageSyncName)
}

// ensureLoadBalancerRequest creates a LoadBalancerRequest for the cluster's
// control plane endpoint if one does not already exist. Only called for cloud
// HA topologies where kube-vip is not available.
func (r *ClusterBootstrapReconciler) ensureLoadBalancerRequest(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) error {
	logger := log.FromContext(ctx)

	name := lbrName(cb)
	lbr := &butlerv1alpha1.LoadBalancerRequest{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: cb.Namespace}, lbr)
	if err == nil {
		return nil // Already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	lbr = &butlerv1alpha1.LoadBalancerRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cb.Namespace,
			Labels: map[string]string{
				"butler.butlerlabs.dev/cluster-bootstrap": cb.Name,
				"butler.butlerlabs.dev/cluster":           cb.Spec.Cluster.Name,
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
		Spec: butlerv1alpha1.LoadBalancerRequestSpec{
			ClusterName: cb.Spec.Cluster.Name,
			ProviderConfigRef: butlerv1alpha1.ProviderReference{
				Name:      cb.Spec.ProviderRef.Name,
				Namespace: cb.Spec.ProviderRef.Namespace,
			},
			Port: 6443,
		},
	}

	if err := r.Create(ctx, lbr); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create LoadBalancerRequest %s: %w", name, err)
	}

	logger.Info("Created LoadBalancerRequest for cloud HA control plane", "name", name)
	return nil
}

// updateLoadBalancerTargets updates the LoadBalancerRequest's target list with
// running control plane machine IPs and instance names. Called each reconcile
// so the provider controller can register new backends as VMs come online.
func (r *ClusterBootstrapReconciler) updateLoadBalancerTargets(ctx context.Context, cb *butlerv1alpha1.ClusterBootstrap) error {
	logger := log.FromContext(ctx)

	name := lbrName(cb)
	lbr := &butlerv1alpha1.LoadBalancerRequest{}
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: cb.Namespace}, lbr); err != nil {
		return fmt.Errorf("failed to get LoadBalancerRequest %s: %w", name, err)
	}

	var targets []butlerv1alpha1.LoadBalancerTarget
	for _, m := range cb.Status.Machines {
		if m.Role != "control-plane" {
			continue
		}
		if m.IPAddress == "" {
			continue
		}
		target := butlerv1alpha1.LoadBalancerTarget{
			IP:           m.IPAddress,
			InstanceName: m.Name,
		}
		// Look up MachineRequest to get provider-specific instance ID
		// (e.g., EC2 instance ID for AWS NLB target registration).
		mr := &butlerv1alpha1.MachineRequest{}
		if err := r.Get(ctx, client.ObjectKey{Name: m.Name, Namespace: cb.Namespace}, mr); err == nil {
			if mr.Status.ProviderID != "" {
				target.InstanceID = mr.Status.ProviderID
			}
		}
		targets = append(targets, target)
	}

	if len(targets) == 0 {
		return nil
	}

	// Only update if targets changed (check IP and InstanceID)
	if len(targets) == len(lbr.Spec.Targets) {
		changed := false
		existing := make(map[string]string) // IP -> InstanceID
		for _, t := range lbr.Spec.Targets {
			existing[t.IP] = t.InstanceID
		}
		for _, t := range targets {
			if existingID, ok := existing[t.IP]; !ok || existingID != t.InstanceID {
				changed = true
				break
			}
		}
		if !changed {
			return nil
		}
	}

	lbr.Spec.Targets = targets
	if err := r.Update(ctx, lbr); err != nil {
		return fmt.Errorf("failed to update LoadBalancerRequest targets: %w", err)
	}

	logger.Info("Updated LoadBalancerRequest targets", "name", name, "targetCount", len(targets))
	return nil
}

func (r *ClusterBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&butlerv1alpha1.ClusterBootstrap{}).
		Owns(&butlerv1alpha1.MachineRequest{}).
		Owns(&butlerv1alpha1.ImageSync{}).
		Owns(&butlerv1alpha1.LoadBalancerRequest{}).
		Complete(r)
}
