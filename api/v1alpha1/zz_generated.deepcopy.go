//go:build !ignore_autogenerated
// +build !ignore_autogenerated

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

// Code generated by controller-gen. DO NOT EDIT.

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SyncthingCluster) DeepCopyInto(out *SyncthingCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SyncthingCluster.
func (in *SyncthingCluster) DeepCopy() *SyncthingCluster {
	if in == nil {
		return nil
	}
	out := new(SyncthingCluster)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *SyncthingCluster) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SyncthingClusterList) DeepCopyInto(out *SyncthingClusterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]SyncthingCluster, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SyncthingClusterList.
func (in *SyncthingClusterList) DeepCopy() *SyncthingClusterList {
	if in == nil {
		return nil
	}
	out := new(SyncthingClusterList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *SyncthingClusterList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SyncthingClusterSpec) DeepCopyInto(out *SyncthingClusterSpec) {
	*out = *in
	in.PodSpec.DeepCopyInto(&out.PodSpec)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SyncthingClusterSpec.
func (in *SyncthingClusterSpec) DeepCopy() *SyncthingClusterSpec {
	if in == nil {
		return nil
	}
	out := new(SyncthingClusterSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SyncthingClusterStatus) DeepCopyInto(out *SyncthingClusterStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]v1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.Nodes != nil {
		in, out := &in.Nodes, &out.Nodes
		*out = make(map[string]SyncthingClusterStatusNode, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SyncthingClusterStatus.
func (in *SyncthingClusterStatus) DeepCopy() *SyncthingClusterStatus {
	if in == nil {
		return nil
	}
	out := new(SyncthingClusterStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SyncthingClusterStatusNode) DeepCopyInto(out *SyncthingClusterStatusNode) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SyncthingClusterStatusNode.
func (in *SyncthingClusterStatusNode) DeepCopy() *SyncthingClusterStatusNode {
	if in == nil {
		return nil
	}
	out := new(SyncthingClusterStatusNode)
	in.DeepCopyInto(out)
	return out
}