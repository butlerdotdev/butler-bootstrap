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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

const azureAPIVersion = "2023-09-01"

// EnsureCloudLBBackendPool ensures management cluster nodes are in the cloud
// LB backend pool. Only Azure requires this — Azure CCM v1.31 with
// vmType=standard creates the LB but does not auto-populate the backend pool.
func (i *Installer) EnsureCloudLBBackendPool(ctx context.Context, kubeconfig []byte, provider string, creds *ProviderCredentials, clusterName string) error {
	if provider != "azure" {
		return nil
	}
	if creds == nil || creds.Azure == nil {
		return fmt.Errorf("Azure credentials required for LB backend pool setup")
	}

	kubeconfigPath, cleanup, err := i.writeKubeconfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	defer cleanup()

	return i.ensureAzureLBBackendPool(ctx, kubeconfigPath, creds.Azure, clusterName)
}

// ensureAzureLBBackendPool adds management cluster node NICs to the Azure LB
// backend pool using the Azure REST API directly (no SDK dependency).
//
// Azure CCM v1.31 with vmType=standard creates the LB rules, public IP, and
// security group entries for LoadBalancer services, but its service controller
// never triggers a backend pool sync after the initial startup. The backend
// pool remains empty, making the LB unreachable.
func (i *Installer) ensureAzureLBBackendPool(ctx context.Context, kubeconfigPath string, creds *AzureCredentials, clusterName string) error {
	logger := log.FromContext(ctx)

	token, err := getAzureToken(ctx, creds.TenantID, creds.ClientID, creds.ClientSecret)
	if err != nil {
		return fmt.Errorf("failed to get Azure token: %w", err)
	}

	// Azure CCM names the LB and backend pool after the cluster name.
	lbName := clusterName
	backendPoolName := clusterName

	baseURL := fmt.Sprintf("https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network",
		creds.SubscriptionID, creds.ResourceGroup)

	// Get LB to find backend pool ID
	lbURL := fmt.Sprintf("%s/loadBalancers/%s?api-version=%s", baseURL, lbName, azureAPIVersion)
	lbBody, err := azureGET(ctx, lbURL, token)
	if err != nil {
		return fmt.Errorf("failed to get LB %s: %w", lbName, err)
	}

	var lb map[string]interface{}
	if err := json.Unmarshal(lbBody, &lb); err != nil {
		return fmt.Errorf("failed to parse LB response: %w", err)
	}

	backendPoolID, err := findBackendPoolID(lb, backendPoolName)
	if err != nil {
		return err
	}

	logger.Info("Found Azure LB backend pool", "lb", lbName, "pool", backendPoolName)

	// List nodes via kubectl to get node names
	nodeNames, err := i.listNodeNames(ctx, kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	for _, nodeName := range nodeNames {
		nicName := fmt.Sprintf("%s-nic", nodeName)
		nicURL := fmt.Sprintf("%s/networkInterfaces/%s?api-version=%s", baseURL, nicName, azureAPIVersion)

		nicBody, err := azureGET(ctx, nicURL, token)
		if err != nil {
			logger.Error(err, "Failed to get NIC, skipping", "nic", nicName)
			continue
		}

		var nic map[string]interface{}
		if err := json.Unmarshal(nicBody, &nic); err != nil {
			logger.Error(err, "Failed to parse NIC response", "nic", nicName)
			continue
		}

		added, err := addBackendPoolToNIC(nic, backendPoolID)
		if err != nil {
			logger.Error(err, "Failed to modify NIC config", "nic", nicName)
			continue
		}
		if !added {
			logger.Info("NIC already in backend pool", "nic", nicName)
			continue
		}

		updatedNIC, err := json.Marshal(nic)
		if err != nil {
			return fmt.Errorf("failed to marshal updated NIC: %w", err)
		}

		if err := azurePUT(ctx, nicURL, token, updatedNIC); err != nil {
			return fmt.Errorf("failed to update NIC %s: %w", nicName, err)
		}

		logger.Info("Added NIC to LB backend pool", "nic", nicName, "pool", backendPoolName)
	}

	return nil
}

func (i *Installer) listNodeNames(ctx context.Context, kubeconfigPath string) ([]string, error) {
	args := []string{
		"--kubeconfig", kubeconfigPath,
		"--insecure-skip-tls-verify",
		"get", "nodes",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`,
	}
	if i.NodeIP != "" {
		args = append(args, "--server", fmt.Sprintf("https://%s:6443", i.NodeIP))
	}
	cmd := exec.CommandContext(ctx, i.KubectlPath, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var names []string
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// findBackendPoolID navigates the LB JSON to find the backend pool ID by name.
func findBackendPoolID(lb map[string]interface{}, poolName string) (string, error) {
	props, ok := lb["properties"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("LB has no properties")
	}
	pools, ok := props["backendAddressPools"].([]interface{})
	if !ok {
		return "", fmt.Errorf("LB has no backendAddressPools")
	}
	for _, p := range pools {
		pool, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := pool["name"].(string)
		id, _ := pool["id"].(string)
		if name == poolName && id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("backend pool %q not found on LB", poolName)
}

// addBackendPoolToNIC adds the backend pool reference to the NIC's first IP
// config. Returns true if the pool was added, false if already present.
func addBackendPoolToNIC(nic map[string]interface{}, backendPoolID string) (bool, error) {
	props, ok := nic["properties"].(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("NIC has no properties")
	}
	ipConfigs, ok := props["ipConfigurations"].([]interface{})
	if !ok || len(ipConfigs) == 0 {
		return false, fmt.Errorf("NIC has no ipConfigurations")
	}
	ipConfig, ok := ipConfigs[0].(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("ipConfigurations[0] is not an object")
	}
	ipConfigProps, ok := ipConfig["properties"].(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("ipConfigurations[0] has no properties")
	}

	// Check existing pools
	var pools []interface{}
	if existing, ok := ipConfigProps["loadBalancerBackendAddressPools"].([]interface{}); ok {
		pools = existing
	}

	for _, p := range pools {
		pool, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if id, _ := pool["id"].(string); id == backendPoolID {
			return false, nil // already present
		}
	}

	pools = append(pools, map[string]interface{}{"id": backendPoolID})
	ipConfigProps["loadBalancerBackendAddressPools"] = pools
	return true, nil
}

// getAzureToken gets an Azure AD access token using service principal credentials.
func getAzureToken(ctx context.Context, tenantID, clientID, clientSecret string) (string, error) {
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID)

	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"scope":         {"https://management.azure.com/.default"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request failed (%s): %s", resp.Status, body)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("empty access token in response")
	}

	return tokenResp.AccessToken, nil
}

// azureGET performs an authenticated GET against the Azure Management API.
func azureGET(ctx context.Context, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Azure API %s: %s", resp.Status, body)
	}

	return body, nil
}

// azurePUT performs an authenticated PUT against the Azure Management API.
func azurePUT(ctx context.Context, reqURL, token string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", reqURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Azure API %s: %s", resp.Status, respBody)
	}

	return nil
}
