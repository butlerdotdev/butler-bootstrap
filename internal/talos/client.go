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

package talos

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/butlerdotdev/butler-bootstrap/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Client implements controller.TalosClientInterface using talosctl
type Client struct {
	TalosctlPath    string
	WorkDir         string
	TalosConfigPath string
}

// Verify interface compliance
var _ controller.TalosClientInterface = &Client{}

// NewClient creates a new Talos client
func NewClient(workDir string) *Client {
	talosctlPath := "talosctl"
	if path := os.Getenv("TALOSCTL_PATH"); path != "" {
		talosctlPath = path
	}

	os.MkdirAll(workDir, 0755)

	return &Client{
		TalosctlPath: talosctlPath,
		WorkDir:      workDir,
	}
}

// GenerateConfig generates Talos machine configurations
func (c *Client) GenerateConfig(ctx context.Context, opts controller.TalosConfigOptions) (*controller.TalosConfigs, error) {
	logger := log.FromContext(ctx)

	outputDir := filepath.Join(c.WorkDir, opts.ClusterName)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory: %w", err)
	}

	endpoint := fmt.Sprintf("https://%s:6443", opts.ControlPlaneVIP)

	args := []string{
		"gen", "config",
		opts.ClusterName,
		endpoint,
		"--output-dir", outputDir,
		"--with-docs=false",
		"--with-examples=false",
	}

	if opts.InstallDisk != "" {
		args = append(args, "--install-disk", opts.InstallDisk)
	}

	if opts.TalosVersion != "" {
		args = append(args, "--talos-version", opts.TalosVersion)
	}

	if opts.PodCIDR != "" {
		args = append(args, "--config-patch", fmt.Sprintf(`[{"op": "add", "path": "/cluster/network/podSubnets", "value": ["%s"]}]`, opts.PodCIDR))
	}

	if opts.ServiceCIDR != "" {
		args = append(args, "--config-patch", fmt.Sprintf(`[{"op": "add", "path": "/cluster/network/serviceSubnets", "value": ["%s"]}]`, opts.ServiceCIDR))
	}

	// Disable default CNI (Cilium will be installed)
	args = append(args, "--config-patch", `[{"op": "add", "path": "/cluster/network/cni", "value": {"name": "none"}}]`)

	// Disable kube-proxy (Cilium handles this)
	args = append(args, "--config-patch", `[{"op": "add", "path": "/cluster/proxy", "value": {"disabled": true}}]`)

	// Allow scheduling on control planes for single-node clusters
	// This enables workloads to run on the single control plane node
	if opts.AllowSchedulingOnControlPlanes {
		logger.Info("Enabling workload scheduling on control planes (single-node mode)")
		args = append(args, "--config-patch", `[{"op": "add", "path": "/cluster/allowSchedulingOnControlPlanes", "value": true}]`)
	}

	// Add custom patches
	for _, patch := range opts.ConfigPatches {
		patchJSON := fmt.Sprintf(`[{"op": "%s", "path": "%s"`, patch.Op, patch.Path)
		if patch.Op != "remove" && patch.Value != "" {
			patchJSON += fmt.Sprintf(`, "value": %s`, patch.Value)
		}
		patchJSON += "}]"
		args = append(args, "--config-patch", patchJSON)
	}

	logger.Info("Running talosctl gen config",
		"cluster", opts.ClusterName,
		"singleNode", opts.AllowSchedulingOnControlPlanes)

	cmd := exec.CommandContext(ctx, c.TalosctlPath, args...)
	cmd.Dir = c.WorkDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("talosctl gen config failed: %w, output: %s", err, string(output))
	}

	configs := &controller.TalosConfigs{
		NodeConfigs: make(map[string][]byte),
	}

	talosConfigPath := filepath.Join(outputDir, "talosconfig")
	if data, err := os.ReadFile(talosConfigPath); err == nil {
		configs.TalosConfig = data
		c.TalosConfigPath = talosConfigPath
	} else {
		return nil, fmt.Errorf("failed to read talosconfig: %w", err)
	}

	cpConfigPath := filepath.Join(outputDir, "controlplane.yaml")
	if data, err := os.ReadFile(cpConfigPath); err == nil {
		configs.ControlPlaneConfig = data
	} else {
		return nil, fmt.Errorf("failed to read controlplane.yaml: %w", err)
	}

	workerConfigPath := filepath.Join(outputDir, "worker.yaml")
	if data, err := os.ReadFile(workerConfigPath); err == nil {
		configs.WorkerConfig = data
	} else {
		return nil, fmt.Errorf("failed to read worker.yaml: %w", err)
	}

	logger.Info("Generated Talos configs", "outputDir", outputDir)
	return configs, nil
}

// ApplyConfig applies a Talos config to a node
func (c *Client) ApplyConfig(ctx context.Context, nodeIP string, config []byte, insecure bool) error {
	logger := log.FromContext(ctx)

	configFile := filepath.Join(c.WorkDir, fmt.Sprintf("config-%s.yaml", strings.ReplaceAll(nodeIP, ".", "-")))
	if err := os.WriteFile(configFile, config, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	defer os.Remove(configFile)

	args := []string{
		"apply-config",
		"--nodes", nodeIP,
		"--file", configFile,
	}

	if insecure {
		args = append(args, "--insecure")
	}

	if c.TalosConfigPath != "" && !insecure {
		args = append(args, "--talosconfig", c.TalosConfigPath)
	}

	logger.Info("Applying Talos config", "node", nodeIP)

	cmd := exec.CommandContext(ctx, c.TalosctlPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("talosctl apply-config failed: %w, output: %s", err, string(output))
	}

	return nil
}

// Bootstrap bootstraps the first control plane node
func (c *Client) Bootstrap(ctx context.Context, nodeIP string) error {
	logger := log.FromContext(ctx)

	args := []string{
		"bootstrap",
		"--nodes", nodeIP,
		"--endpoints", nodeIP,
	}

	if c.TalosConfigPath != "" {
		args = append(args, "--talosconfig", c.TalosConfigPath)
	}

	logger.Info("Running talosctl bootstrap", "node", nodeIP)

	cmd := exec.CommandContext(ctx, c.TalosctlPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "already bootstrapped") ||
			strings.Contains(string(output), "etcd data directory is not empty") {
			logger.Info("Cluster already bootstrapped")
			return nil
		}
		return fmt.Errorf("talosctl bootstrap failed: %w, output: %s", err, string(output))
	}

	return nil
}

// GetKubeconfig retrieves the kubeconfig
func (c *Client) GetKubeconfig(ctx context.Context, nodeIP string) ([]byte, error) {
	logger := log.FromContext(ctx)

	kubeconfigPath := filepath.Join(c.WorkDir, "kubeconfig")

	args := []string{
		"kubeconfig",
		"--nodes", nodeIP,
		"--endpoints", nodeIP,
		"--force",
		kubeconfigPath,
	}

	if c.TalosConfigPath != "" {
		args = append(args, "--talosconfig", c.TalosConfigPath)
	}

	logger.Info("Retrieving kubeconfig", "node", nodeIP)

	cmd := exec.CommandContext(ctx, c.TalosctlPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("talosctl kubeconfig failed: %w, output: %s", err, string(output))
	}

	kubeconfig, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read kubeconfig: %w", err)
	}

	return kubeconfig, nil
}

// WaitForNodeReady waits for a node to be ready
func (c *Client) WaitForNodeReady(ctx context.Context, nodeIP string, timeout time.Duration) error {
	logger := log.FromContext(ctx)

	args := []string{
		"health",
		"--nodes", nodeIP,
		"--endpoints", nodeIP,
		"--wait-timeout", timeout.String(),
	}

	if c.TalosConfigPath != "" {
		args = append(args, "--talosconfig", c.TalosConfigPath)
	}

	logger.Info("Waiting for node health", "node", nodeIP, "timeout", timeout)

	cmd := exec.CommandContext(ctx, c.TalosctlPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("talosctl health failed: %w, output: %s", err, string(output))
	}

	logger.Info("Node is healthy", "node", nodeIP)
	return nil
}

// SetTalosConfigFromBytes writes talosconfig to disk and sets the path
func (c *Client) SetTalosConfigFromBytes(config []byte) error {
	path := filepath.Join(c.WorkDir, "talosconfig")
	if err := os.WriteFile(path, config, 0600); err != nil {
		return fmt.Errorf("failed to write talosconfig: %w", err)
	}
	c.TalosConfigPath = path
	return nil
}
