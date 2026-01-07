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

// Package crds provides embedded Butler CRD manifests for deployment to target clusters.
package crds

import (
	"embed"
)

// PlatformCRDs contains Butler platform CRDs that need to be installed on management clusters.
// These are the CRDs that butler-controller watches (TenantCluster, Team, etc.)
//
// Note: Bootstrap CRDs (ClusterBootstrap, MachineRequest, ProviderConfig) are only needed
// in the temporary KIND cluster and are handled separately by butler-cli.
//
//go:embed *.yaml
var PlatformCRDs embed.FS
