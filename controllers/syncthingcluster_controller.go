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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	stcsamcdaycomv1alpha1 "github.com/samcday/stc/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type SyncthingClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type SyncthingRestTransport struct {
	http.RoundTripper
}

func (srt *SyncthingRestTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	req.Header.Set("X-API-Key", "boobies")
	return srt.RoundTripper.RoundTrip(req)
}

const finalizerName = "stc.samcday.com/finalizer"

//+kubebuilder:rbac:groups=stc.samcday.com,resources=syncthingclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=stc.samcday.com,resources=syncthingclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=stc.samcday.com,resources=syncthingclusters/finalizers,verbs=update

//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete

func (r *SyncthingClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("name", req.Name)

	var c stcsamcdaycomv1alpha1.SyncthingCluster
	err := r.Client.Get(ctx, req.NamespacedName, &c)
	if err != nil {
		log.Error(err, "failed to get SyncthingCluster")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	hasFinalizer := controllerutil.ContainsFinalizer(&c, finalizerName)
	isDeleted := !c.ObjectMeta.DeletionTimestamp.IsZero()

	if isDeleted && !hasFinalizer {
		// This object is in the process of being deleted, and does not have our finalizer.
		// Nothing to do here.
		return ctrl.Result{}, nil
	}

	var ds appsv1.DaemonSet
	err = r.Client.Get(ctx, req.NamespacedName, &ds)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	dsPatch := client.MergeFrom(ds.DeepCopy())
	dsExists := ds.Name != ""

	owner := metav1.GetControllerOf(&ds)
	if dsExists && (owner == nil || owner.APIVersion != stcsamcdaycomv1alpha1.GroupVersion.String() || owner.Kind != c.Kind) {
		// TODO: handle this properly.
		return ctrl.Result{}, fmt.Errorf("an unmanaged DaemonSet already exists")
	}

	ds.Name = req.Name
	ds.Namespace = req.Namespace
	r.ensureDaemonSet(&ds, &c)

	if dsExists {
		log.V(1).Info("Patched DaemonSet for SyncthingCluster", "name", req.Name)
		err = r.Patch(ctx, &ds, dsPatch)
	} else {
		log.V(1).Info("Created DaemonSet for SyncthingCluster", "name", req.Name)
		if err := ctrl.SetControllerReference(&c, &ds, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		err = r.Create(ctx, &ds)
	}

	log.V(1).Info("DaemonSet reconciled", "generation", ds.Generation)

	dss := ds.Status
	if dss.ObservedGeneration < ds.Generation || dss.NumberReady < dss.DesiredNumberScheduled || dss.UpdatedNumberScheduled < dss.DesiredNumberScheduled {
		log.V(1).Info("DaemonSet not yet up to date")
		return ctrl.Result{}, r.updateReadiness(ctx, &c, false, "DaemonSetNotUpToDate", "DaemonSet has not fully rolled out yet")
	}

	if !hasFinalizer {
		// Cluster is not being deleted, but does not yet have finalizer. Add it now.
		controllerutil.AddFinalizer(&c, finalizerName)
		if err := r.Update(ctx, &c); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Enumerate the created Syncthing pods, ping the REST status API of each.
	var pods corev1.PodList
	err = r.List(ctx, &pods, client.InNamespace(req.Namespace), client.MatchingFields{"daemonset-owner": ds.Name})
	if err != nil {
		return ctrl.Result{}, err
	}

	cli := &http.Client{
		Transport: &SyncthingRestTransport{RoundTripper: http.DefaultTransport},
	}
	c.Status.Nodes = make(map[string]stcsamcdaycomv1alpha1.SyncthingClusterStatusNode)
	for _, pod := range pods.Items {
		var versionResp struct {
			Version string `json:"version,omitempty"`
		}
		var statusResp struct {
			ID string `json:"myID,omitempty"`
		}

		resp, err := cli.Get(fmt.Sprintf("http://%s:8384/rest/system/version", pod.Status.PodIP))
		if err != nil {
			return ctrl.Result{}, err
		}
		err = json.NewDecoder(resp.Body).Decode(&versionResp)
		if err != nil {
			return ctrl.Result{}, err
		}
		resp, err = cli.Get(fmt.Sprintf("http://%s:8384/rest/system/status", pod.Status.PodIP))
		if err != nil {
			return ctrl.Result{}, err
		}
		err = json.NewDecoder(resp.Body).Decode(&statusResp)
		if err != nil {
			return ctrl.Result{}, err
		}

		c.Status.Nodes[pod.Spec.NodeName] = stcsamcdaycomv1alpha1.SyncthingClusterStatusNode{
			DeviceID: statusResp.ID,
			Version:  versionResp.Version,
		}
	}

	err = r.Status().Update(ctx, &c)
	if err != nil {
		return ctrl.Result{}, err
	}

	// All ready pods have been successfully pinged via REST API.
	//isReady := false

	if isDeleted {
		// Check that Syncthing cluster is empty before removing the finalizer.
		return ctrl.Result{}, fmt.Errorf("not implemented yet")
	}

	return ctrl.Result{}, r.updateReadiness(ctx, &c, true, "InSync", "TODO")
}

func (r *SyncthingClusterReconciler) ensureDaemonSet(ds *appsv1.DaemonSet, c *stcsamcdaycomv1alpha1.SyncthingCluster) {
	appLabels := map[string]string{
		"app.kubernetes.io/name":      "stc",
		"app.kubernetes.io/instance":  ds.Name,
		"app.kubernetes.io/component": "syncthing",
	}

	ds.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: appLabels,
	}
	dsTmpl := &ds.Spec.Template
	dsTmpl.Labels = appLabels
	dsTmpl.Spec = *c.Spec.PodSpec.DeepCopy()
	podSpec := &dsTmpl.Spec
	podSpec.RestartPolicy = corev1.RestartPolicyAlways

	if podSpec.Affinity == nil {
		podSpec.Affinity = &corev1.Affinity{}
	}
	aff := podSpec.Affinity

	if aff.NodeAffinity == nil {
		aff.NodeAffinity = &corev1.NodeAffinity{}
	}
	nodeAff := aff.NodeAffinity
	if nodeAff.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		nodeAff.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}

	// Ensure node affinity includes support Syncthing platforms (amd64, arm64, arm)
	rdside := nodeAff.RequiredDuringSchedulingIgnoredDuringExecution
	rdside.NodeSelectorTerms = append(rdside.NodeSelectorTerms, corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{
				Key:      "kubernetes.io/arch",
				Operator: "In",
				Values:   []string{"amd64", "arm64", "arm"},
			},
		},
	})

	// Ensure pod has a "syncthing-data" volume, which is a hostPath mount.
	var vol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "syncthing-data" {
			vol = &podSpec.Volumes[i]
			break
		}
	}
	if vol == nil {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{})
		vol = &podSpec.Volumes[len(podSpec.Volumes)-1]
	}
	hostPath := ""
	if vol.VolumeSource.HostPath != nil {
		hostPath = vol.VolumeSource.HostPath.Path
	}
	if hostPath == "" {
		hostPath = "/var/syncthing/" + string(c.UID)
	}
	hostPathType := corev1.HostPathDirectoryOrCreate
	*vol = corev1.Volume{
		Name: "syncthing-data",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Type: &hostPathType,
				Path: hostPath,
			},
		},
	}

	// Ensure pod has a "syncthing" container.
	var ctr *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == "syncthing" {
			ctr = &podSpec.Containers[i]
			break
		}
	}
	if ctr == nil {
		podSpec.Containers = append(podSpec.Containers, corev1.Container{
			Name: "syncthing",
		})
		ctr = &podSpec.Containers[len(podSpec.Containers)-1]
	}

	if ctr.Image == "" {
		ctr.Image = "syncthing/syncthing:1"
	}

	// Ensure syncthing container has a /var/syncthing volume mount referencing syncthing-data volume.
	var mnt *corev1.VolumeMount
	for i := range ctr.VolumeMounts {
		if ctr.VolumeMounts[i].MountPath == "/var/syncthing" {
			mnt = &ctr.VolumeMounts[i]
			break
		}
	}
	if mnt == nil {
		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{
			MountPath: "/var/syncthing",
		})
		mnt = &ctr.VolumeMounts[len(ctr.VolumeMounts)-1]
	}
	mnt.Name = "syncthing-data"
}

func (r *SyncthingClusterReconciler) updateReadiness(ctx context.Context, c *stcsamcdaycomv1alpha1.SyncthingCluster, isReady bool, reason, message string) error {
	readyStatus := metav1.ConditionFalse
	if isReady {
		readyStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  readyStatus,
		Reason:  reason,
		Message: message,
	})

	return r.Status().Update(ctx, c)
}

func (r *SyncthingClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "daemonset-owner", func(o client.Object) []string {
		pod := o.(*corev1.Pod)
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" {
			return nil
		}
		return []string{owner.Name}
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&stcsamcdaycomv1alpha1.SyncthingCluster{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}
