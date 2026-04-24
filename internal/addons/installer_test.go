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

import "testing"

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		name     string
		ref      string
		wantRepo string
		wantTag  string
	}{
		{
			name:     "empty",
			ref:      "",
			wantRepo: "",
			wantTag:  "",
		},
		{
			name:     "repo and tag only",
			ref:      "repo:tag",
			wantRepo: "repo",
			wantTag:  "tag",
		},
		{
			name:     "registry with repo and tag",
			ref:      "registry.io/repo:tag",
			wantRepo: "registry.io/repo",
			wantTag:  "tag",
		},
		{
			name:     "registry with port, repo, and tag",
			ref:      "registry.io:5000/repo:tag",
			wantRepo: "registry.io:5000/repo",
			wantTag:  "tag",
		},
		{
			name:     "no tag",
			ref:      "registry.io/repo",
			wantRepo: "registry.io/repo",
			wantTag:  "",
		},
		{
			// Digest references are not supported: splitImageRef splits on the
			// last colon in the tail, which for a digest ref falls inside the
			// digest itself. Callers must pass tag-based refs. Bootstrap's
			// GetButlerControllerImage() always produces "repo:tag" form, so
			// this case is a contract statement, not a supported input.
			name:     "digest ref is not supported (documented)",
			ref:      "ghcr.io/butlerdotdev/butler-controller@sha256:abc",
			wantRepo: "ghcr.io/butlerdotdev/butler-controller@sha256",
			wantTag:  "abc",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRepo, gotTag := splitImageRef(tc.ref)
			if gotRepo != tc.wantRepo || gotTag != tc.wantTag {
				t.Errorf("splitImageRef(%q) = (%q, %q); want (%q, %q)",
					tc.ref, gotRepo, gotTag, tc.wantRepo, tc.wantTag)
			}
		})
	}
}
