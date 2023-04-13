/*
Copyright 2023 Sam Day.

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type SyncthingClusterSpec struct {
	PodSpec corev1.PodSpec `json:"podSpec,omitempty"`
}

type SyncthingClusterStatusNode struct {
	DeviceID string `json:"deviceID,omitempty"`
	Version  string `json:"version,omitempty"`
}

type SyncthingClusterStatus struct {
	Conditions []metav1.Condition                    `json:"conditions,omitempty"`
	Nodes      map[string]SyncthingClusterStatusNode `json:"nodes,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

type SyncthingCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SyncthingClusterSpec   `json:"spec,omitempty"`
	Status SyncthingClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

type SyncthingClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SyncthingCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SyncthingCluster{}, &SyncthingClusterList{})
}
