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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	stc "github.com/samcday/stc/api/v1alpha1"
	stconfig "github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	klog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"time"
)

type SyncthingClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type SyncthingTransport struct {
	http.RoundTripper
	APIKey string
}

func (srt *SyncthingTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	req.Header.Set("X-API-Key", srt.APIKey)
	return srt.RoundTripper.RoundTrip(req)
}

const finalizerName = "stc.samcday.com/finalizer"

//+kubebuilder:rbac:groups=stc.samcday.com,resources=syncthingclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=stc.samcday.com,resources=syncthingclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=stc.samcday.com,resources=syncthingclusters/finalizers,verbs=update

//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete

func (r *SyncthingClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx)

	var c stc.SyncthingCluster
	err := r.Client.Get(ctx, req.NamespacedName, &c)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(client.IgnoreNotFound(err), "failed to get SyncthingCluster")
	}

	hasFinalizer := controllerutil.ContainsFinalizer(&c, finalizerName)
	isDeleted := !c.ObjectMeta.DeletionTimestamp.IsZero()

	if isDeleted && !hasFinalizer {
		// This object is being deleted, and does not have our finalizer. Nothing to do here.
		return ctrl.Result{}, nil
	}

	dsReady, err := r.ensureDaemonSet(log, ctx, req, &c)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to reconcile DaemonSet for '%s'", req.Name)
	}

	if !hasFinalizer {
		// Cluster is not being deleted, but does not yet have finalizer. Add it now.
		controllerutil.AddFinalizer(&c, finalizerName)
		log.V(1).Info("adding finalizer")
		err := r.Update(ctx, &c)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to add finalizer to '%s'", req.Name)
		}
	}

	if !dsReady {
		// DaemonSet isn't ready, reconciliation cannot proceed to Syncthing configuration phase.
		return reconcile.Result{}, nil
	}

	// Enumerate the Syncthing pods, ping the REST status API of each.
	var pods corev1.PodList
	err = r.List(ctx, &pods, client.InNamespace(req.Namespace), client.MatchingFields{"daemonset-owner": req.Name})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to list pods")
	}

	isReady := true

	nodesByIP := make(map[string]*stc.SyncthingClusterStatusNode)
	clients := make(map[string]*http.Client)

	c.Status.Nodes = make(map[string]*stc.SyncthingClusterStatusNode)
	for _, pod := range pods.Items {
		ip := pod.Status.PodIP
		client := &http.Client{Transport: &SyncthingTransport{APIKey: string(pod.UID), RoundTripper: http.DefaultTransport}}
		clients[ip] = client
		nodeStatus, err := r.syncthingStatus(client, ip)
		nodesByIP[ip] = nodeStatus
		c.Status.Nodes[pod.Spec.NodeName] = nodeStatus
		if err != nil {
			isReady = false
			nodeStatus.LastErrorTime = metav1.NewTime(time.Now())
			nodeStatus.LastError = err.Error()
			continue
		}
	}

	label := "cluster.stc.samcday.com/" + string(c.UID)

	// Ensure all Nodes running Syncthing instances for this cluster are labelled.
	for _, pod := range pods.Items {
		var node corev1.Node
		err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to lookup Node "+pod.Spec.NodeName)
		}
		if _, ok := node.Labels[label]; !ok {
			node.Labels[label] = ""
			err := r.Update(ctx, &node)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to update Node")
			}
		}
	}
	// Ensure label is not on any Nodes where it shouldn't be.
	var nodes corev1.NodeList
	err = r.List(ctx, &nodes, client.MatchingFields{"cluster": string(c.UID)})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to list Nodes")
	}
	for _, n := range nodes.Items {
		remove := true
		for k := range c.Status.Nodes {
			if n.Name == k {
				remove = false
				break
			}
		}
		if remove {
			delete(n.Labels, label)
			err := r.Update(ctx, &n)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to update Node")
			}
		}
	}

	if isReady {
		// All Syncthing instances are running and appear healthy. Ensure they are configured according to spec.
		for ip, nodeStatus := range nodesByIP {
			err := r.ensureSyncthingConfig(log, clients[ip], ip, nodeStatus.DeviceID, &c)
			if err != nil {
				isReady = false
				nodeStatus.LastErrorTime = metav1.NewTime(time.Now())
				nodeStatus.LastError = err.Error()
				continue
			}
			nodeStatus.Ready = true
		}
	}

	if isDeleted {
		// TODO: Check that Syncthing cluster is empty before removing the finalizer.
		return ctrl.Result{}, r.updateReadiness(log, ctx, &c, false, "Deleting", "Cluster is being deleted (TODO, manually remove the finalizer for now)")
	}

	reason := "Waiting"
	message := "Waiting for all cluster peers to become ready..."
	result := reconcile.Result{RequeueAfter: 5 * time.Second}

	if isReady {
		reason = "Reconciled"
		message = "Cluster is online and configured."
		result = reconcile.Result{RequeueAfter: time.Minute}
	}

	err = r.updateReadiness(log, ctx, &c, isReady, reason, message)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to update readiness")
	}
	return result, nil
}

func stAPI(stClient *http.Client, ip, path string, out interface{}) (resp *http.Response, err error) {
	resp, err = stClient.Get(fmt.Sprintf("http://%s:8384/rest/%s", ip, path))
	if err != nil {
		return
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		err = json.NewDecoder(resp.Body).Decode(out)
	}
	return
}

func (r *SyncthingClusterReconciler) ensureDaemonSet(log logr.Logger, ctx context.Context, req reconcile.Request, c *stc.SyncthingCluster) (bool, error) {
	var ds appsv1.DaemonSet
	err := r.Client.Get(ctx, req.NamespacedName, &ds)
	if err != nil && !k8serr.IsNotFound(err) {
		return false, err
	}

	dsPatch := client.MergeFrom(ds.DeepCopy())
	dsExists := ds.Name != ""

	owner := metav1.GetControllerOf(&ds)
	if dsExists && (owner == nil || owner.APIVersion != stc.GroupVersion.String() || owner.Kind != c.Kind) {
		return false, r.updateReadiness(log, ctx, c, false,
			"Error", fmt.Sprintf("An unmanaged DaemonSet with the name '%s' already exists", req.Name))
	}

	// Ensure baseline DaemonSet metadata (name, namespace, label selector, pod template from config)
	ds.Name = req.Name
	ds.Namespace = req.Namespace
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

	// Ensure node affinity includes supported Syncthing platforms (amd64, arm64, arm)
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

	// Ensure syncthing container has STGUIAPIKEY set.
	for i, e := range ctr.Env {
		if e.Name == "STGUIAPIKEY" {
			ctr.Env[i] = ctr.Env[len(ctr.Env)-1]
			ctr.Env = ctr.Env[:len(ctr.Env)-1]
			break
		}
	}
	ctr.Env = append(ctr.Env, corev1.EnvVar{
		Name:      "STGUIAPIKEY",
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}},
	})

	if dsExists {
		err = r.Patch(ctx, &ds, dsPatch)
	} else {
		if err := ctrl.SetControllerReference(c, &ds, r.Scheme); err != nil {
			return false, err
		}
		err = r.Create(ctx, &ds)
	}

	dss := ds.Status
	if dss.ObservedGeneration < ds.Generation || dss.NumberReady < dss.DesiredNumberScheduled || dss.UpdatedNumberScheduled < dss.DesiredNumberScheduled {
		return false, r.updateReadiness(log, ctx, c, false, "DaemonSetNotUpToDate", "DaemonSet has not fully rolled out yet")
	}

	return true, nil
}

func (r *SyncthingClusterReconciler) updateReadiness(log logr.Logger, ctx context.Context, c *stc.SyncthingCluster, isReady bool, reason, message string) error {
	readyStatus := metav1.ConditionFalse
	if isReady {
		readyStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             readyStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: c.Generation,
	})
	log.V(1).Info("updating readiness", "status", readyStatus, "message", message, "reason", reason)
	return r.Status().Update(ctx, c)
}

func (r *SyncthingClusterReconciler) syncthingStatus(client *http.Client, ip string) (nodeStatus *stc.SyncthingClusterStatusNode, err error) {
	var versionResp struct {
		Version string `json:"version,omitempty"`
	}
	var statusResp struct {
		ID string `json:"myID,omitempty"`
	}
	var deviceResp map[string]struct {
		LastSeen metav1.Time `json:"lastSeen"`
	}
	var folderResp map[string]struct {
		LastScan metav1.Time `json:"lastScan"`
	}

	nodeStatus = &stc.SyncthingClusterStatusNode{
		Devices: map[string]metav1.Time{},
		Folders: map[string]metav1.Time{},
	}

	_, err = stAPI(client, ip, "system/version", &versionResp)
	if err != nil {
		return
	}
	nodeStatus.Online = true
	nodeStatus.Version = versionResp.Version

	_, err = stAPI(client, ip, "system/status", &statusResp)
	if err != nil {
		return
	}
	nodeStatus.DeviceID = statusResp.ID

	_, err = stAPI(client, ip, "stats/device", &deviceResp)
	if err != nil {
		return
	}
	for k, v := range deviceResp {
		if k == nodeStatus.DeviceID {
			continue
		}
		nodeStatus.Devices[k] = v.LastSeen
	}

	_, err = stAPI(client, ip, "stats/folder", &folderResp)
	if err != nil {
		return
	}
	for k, v := range folderResp {
		nodeStatus.Folders[k] = v.LastScan
	}

	return
}

func (r *SyncthingClusterReconciler) ensureSyncthingConfig(log logr.Logger, stClient *http.Client, ip, myID string, c *stc.SyncthingCluster) error {
	var updateDevices []stconfig.DeviceConfiguration

	resp, err := stClient.Get(fmt.Sprintf("http://%s:8384/rest/config", ip))
	if err != nil {
		panic(err)
	}
	cfg, err := stconfig.ReadJSON(resp.Body, mustDID(myID))
	if err != nil {
		panic(err)
	}

	for nodeName, nodeStatus := range c.Status.Nodes {
		dev, _, found := cfg.Device(mustDID(nodeStatus.DeviceID))
		if !found || dev.Name != nodeName {
			dev.Name = nodeName
			dev.DeviceID = mustDID(nodeStatus.DeviceID)
			updateDevices = append(updateDevices, dev)
		}
	}

	for deviceID, req := range c.Spec.Devices {
		deviceID := mustDID(deviceID)
		dev, _, found := cfg.Device(deviceID)
		if !found || dev.AutoAcceptFolders != req.AutoAcceptFolders || dev.Introducer != req.Introducer {
			dev.DeviceID = deviceID
			dev.Introducer = req.Introducer
			dev.AutoAcceptFolders = req.AutoAcceptFolders
			updateDevices = append(updateDevices, dev)
		}
	}

	if len(updateDevices) > 0 {
		log.V(1).Info("Updating devices", "ip", ip, "count", len(updateDevices))
		body, err := json.Marshal(updateDevices)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("PUT", fmt.Sprintf("http://%s:8384/rest/config/devices", ip), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := stClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("device update HTTP error: %d", resp.StatusCode)
		}
	}
	return nil
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

	err = mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Node{}, "cluster", func(o client.Object) []string {
		node := o.(*corev1.Node)
		var ids []string
		for k := range node.Labels {
			if strings.HasPrefix(k, "cluster.stc.samcday.com/") {
				ids = append(ids, strings.TrimPrefix(k, "cluster.stc.samcday.com/"))
			}
		}
		return ids
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&stc.SyncthingCluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}

func mustDID(raw string) protocol.DeviceID {
	did, err := protocol.DeviceIDFromString(raw)
	if err != nil {
		panic(err)
	}
	return did
}
