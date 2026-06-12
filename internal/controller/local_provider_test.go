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
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func TestInClusterKubeconfig(t *testing.T) {
	r := &ClusterBootstrapReconciler{
		RestConfig: &rest.Config{
			Host:        "https://10.96.0.1:443",
			BearerToken: "test-token",
			TLSClientConfig: rest.TLSClientConfig{
				CAData: []byte("test-ca"),
			},
		},
	}

	data, err := r.inClusterKubeconfig()
	if err != nil {
		t.Fatalf("inClusterKubeconfig: %v", err)
	}

	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("generated kubeconfig does not parse: %v", err)
	}

	cluster := cfg.Clusters[cfg.CurrentContext]
	if cluster == nil {
		t.Fatalf("no cluster for current context %q", cfg.CurrentContext)
	}
	if cluster.Server != "https://10.96.0.1:443" {
		t.Errorf("server = %q, want https://10.96.0.1:443", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != "test-ca" {
		t.Errorf("CA data = %q, want test-ca", cluster.CertificateAuthorityData)
	}

	authInfo := cfg.AuthInfos[cfg.CurrentContext]
	if authInfo == nil || authInfo.Token != "test-token" {
		t.Errorf("token not propagated into kubeconfig")
	}
}

func TestInClusterKubeconfig_NoConfig(t *testing.T) {
	r := &ClusterBootstrapReconciler{}
	if _, err := r.inClusterKubeconfig(); err == nil {
		t.Error("expected error when RestConfig is nil")
	}
}
