/*
Copyright 2024 The Kubernetes Authors.

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

package gce

import (
	"errors"
	"fmt"

	gce "google.golang.org/api/compute/v1"
	"sigs.k8s.io/yaml"
)

const (
	kubeEnvKey = "kube-env"
)

// KubeEnv stores kube-env information from InstanceTemplate
type KubeEnv map[string]string

// ExtractKubeEnv extracts kube-env from InstanceTemplate
func ExtractKubeEnv(template *gce.InstanceTemplate) (KubeEnv, error) {
	if template == nil {
		return nil, errors.New("instance template is nil")
	}
	if template.Properties == nil || template.Properties.Metadata == nil {
		return nil, fmt.Errorf("instance template %s has no metadata", template.Name)
	}
	for _, item := range template.Properties.Metadata.Items {
		if item.Key == kubeEnvKey {
			if item.Value == nil {
				return nil, fmt.Errorf("no kube-env content in metadata")
			}
			return ParseKubeEnv(*item.Value)
		}
	}
	return nil, nil
}

// ParseKubeEnv parses kube-env from its string representation
func ParseKubeEnv(kubeEnvValue string) (KubeEnv, error) {
	kubeEnv := make(map[string]string)
	err := yaml.Unmarshal([]byte(kubeEnvValue), &kubeEnv)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling kubeEnv: %v", err)
	}
	return kubeEnv, nil
}

// Var extracts variable from KubeEnv
func (ke KubeEnv) Var(name string) (string, bool) {
	if ke == nil {
		return "", false
	}
	val, found := ke[name]
	return val, found
}
