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
	stc "code.samcday.com/me/stc/api/v1alpha1"
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	stconfig "github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"net/http"
	"os"
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

//+kubebuilder:rbac:groups=storage.k8s.io,resources=csidrivers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;update;patch;delete

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

	ds, err := r.ensureDaemonSet(log, ctx, &c)
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

	if ds == nil {
		// DaemonSet isn't ready, reconciliation cannot proceed to Syncthing configuration phase.
		return reconcile.Result{}, nil
	}

	// Enumerate the Syncthing pods, ping the REST status API of each.
	var pods corev1.PodList
	err = r.List(ctx, &pods, client.InNamespace(req.Namespace), client.MatchingFields{"daemonset-owner": ds.Name})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to list pods")
	}

	isReady := true

	nodesByIP := make(map[string]*stc.SyncthingClusterStatusNode)
	clients := make(map[string]*http.Client)
	clientIPs := make(map[string]string)

	c.Status.Nodes = make(map[string]*stc.SyncthingClusterStatusNode)
	for _, pod := range pods.Items {
		ip := pod.Status.PodIP
		cli := &http.Client{Transport: &SyncthingTransport{APIKey: string(pod.UID), RoundTripper: http.DefaultTransport}}
		clients[ip] = cli
		nodeStatus, err := r.syncthingStatus(cli, ip)
		nodesByIP[ip] = nodeStatus
		clientIPs[pod.Spec.NodeName] = ip
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
		if isReady {
			for name, v := range c.Status.Nodes {
				ip := clientIPs[name]
				var completion struct {
					Completion  float64 `json:"completion"`
					GlobalBytes int     `json:"globalBytes"`
					GlobalItems int     `json:"globalItems"`
				}
				_, err := stAPI(clients[ip], ip, "/rest/db/completion?device="+string(v.DeviceID), &completion)
				if err != nil {
					return ctrl.Result{}, err
				}
				if completion.GlobalItems > 0 || completion.GlobalBytes > 0 {
					return ctrl.Result{}, r.updateReadiness(ctx, log, &c, false, "Deleting", fmt.Sprintf("Node %s (%s) still has %d local files / %d local bytes",
						name, v.DeviceID, completion.GlobalItems, completion.GlobalBytes))
				}
			}

			// Ensure CSIDriver + StorageClass are deleted.
			var drv storagev1.CSIDriver
			err := r.Get(ctx, types.NamespacedName{Name: csiDriverName(&c)}, &drv)
			if err != nil && !k8serr.IsNotFound(err) {
				return ctrl.Result{}, errors.Wrap(err, "failed to get CSIDriver")
			}
			if drv.Name != "" {
				err := r.Delete(ctx, &drv)
				if err != nil {
					return ctrl.Result{}, errors.Wrap(err, "failed to delete CSIDriver")
				}
			}
			var sc storagev1.StorageClass
			err = r.Get(ctx, types.NamespacedName{Name: storageClassName(&c)}, &sc)
			if err != nil && !k8serr.IsNotFound(err) {
				return ctrl.Result{}, errors.Wrap(err, "failed to get StorageClass")
			}
			if sc.Name != "" {
				err := r.Delete(ctx, &sc)
				if err != nil {
					return ctrl.Result{}, errors.Wrap(err, "failed to delete StorageClass")
				}
			}

			controllerutil.RemoveFinalizer(&c, finalizerName)
			return ctrl.Result{}, r.Update(ctx, &c)
		} else {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, r.updateReadiness(ctx, log, &c, false, "Deleting", "Waiting for cluster to be ready, before deleting it")
		}
	}

	err = r.ensureCSIDriver(ctx, log, &c)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to reconcile CSIDriver")
	}

	err = r.ensureStorageClass(ctx, log, &c)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to reconcile StorageClass")
	}

	reason := "Waiting"
	message := "Waiting for all cluster peers to become ready..."
	result := reconcile.Result{RequeueAfter: 5 * time.Second}

	if isReady {
		reason = "Reconciled"
		message = "Cluster is online and configured."
		result = reconcile.Result{RequeueAfter: time.Minute}
	}

	err = r.updateReadiness(ctx, log, &c, isReady, reason, message)
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

func (r *SyncthingClusterReconciler) ensureDaemonSet(log logr.Logger, ctx context.Context, c *stc.SyncthingCluster) (*appsv1.DaemonSet, error) {
	var ds appsv1.DaemonSet
	dsName := fmt.Sprintf("stc-%s", c.Name)
	err := r.Client.Get(ctx, types.NamespacedName{Name: dsName, Namespace: c.Namespace}, &ds)
	if err != nil && !k8serr.IsNotFound(err) {
		return nil, err
	}

	dsPatch := client.MergeFrom(ds.DeepCopy())
	dsExists := ds.Name != ""

	owner := metav1.GetControllerOf(&ds)
	if dsExists && (owner == nil || owner.APIVersion != stc.GroupVersion.String() || owner.Kind != c.Kind) {
		return nil, r.updateReadiness(ctx, log, c, false, "Error", fmt.Sprintf("An unmanaged DaemonSet with the name '%s' already exists", dsName))
	}

	// Ensure baseline DaemonSet metadata (name, namespace, label selector, pod template from config)
	ds.Name = dsName
	ds.Namespace = c.Namespace
	if ds.Labels == nil {
		ds.Labels = map[string]string{}
	}
	appLabels := map[string]string{
		"app.kubernetes.io/name":      "stc",
		"app.kubernetes.io/instance":  ds.Name,
		"app.kubernetes.io/component": "syncthing",
	}
	ds.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{},
	}
	dsTmpl := &ds.Spec.Template
	dsTmpl.Labels = map[string]string{}
	for k, v := range appLabels {
		dsTmpl.Labels[k] = v
		ds.Spec.Selector.MatchLabels[k] = v
	}
	if c.Spec.PodSpec != nil {
		dsTmpl.Spec = *c.Spec.PodSpec.DeepCopy()
	} else {
		dsTmpl.Spec = corev1.PodSpec{}
	}
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

	ensurePodHostPathVolume(podSpec, "kubelet", "/var/lib/kubelet", corev1.HostPathDirectory)
	ensurePodHostPathVolume(podSpec, "csi", "/var/lib/kubelet/plugins/"+string(c.UID)+".stc.samcday.com", corev1.HostPathDirectoryOrCreate)
	ensurePodHostPathVolume(podSpec, "registration", "/var/lib/kubelet/plugins_registry", corev1.HostPathDirectory)
	ensurePodHostPathVolume(podSpec, "dev", "/dev", corev1.HostPathDirectory)

	// Ensure pod has a "syncthing-data" volume, which is a hostPath mount.
	vol := ensurePodVolume(podSpec, "syncthing-data")
	hostPath := "/var/syncthing/" + string(c.UID)
	if vol.VolumeSource.HostPath != nil && vol.VolumeSource.HostPath.Path != "" {
		hostPath = vol.VolumeSource.HostPath.Path
	}
	hostPathType := corev1.HostPathDirectoryOrCreate
	vol.VolumeSource = corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{
			Type: &hostPathType,
			Path: hostPath,
		},
	}

	// Ensure pod has a "syncthing" container.
	ctr := ensurePodContainer(podSpec, "syncthing")
	if ctr.Image == "" {
		ctr.Image = "syncthing/syncthing:1"
	}

	// TODO: this was resulting in permission denied" issues during folder sync
	// So for now syncthing is run as root inside the container.
	ctr.Command = []string{"/bin/syncthing", "-home", "/var/syncthing/config"}
	//if ctr.SecurityContext == nil {
	//	ctr.SecurityContext = &corev1.SecurityContext{}
	//}
	//if ctr.SecurityContext.Capabilities == nil {
	//	ctr.SecurityContext.Capabilities = &corev1.Capabilities{}
	//}
	//ensureCap := func(caps *corev1.Capabilities, name string) {
	//	for _, c := range caps.Add {
	//		if string(c) == name {
	//			return
	//		}
	//	}
	//	caps.Add = append(caps.Add, corev1.Capability(name))
	//}
	//ensureCap(ctr.SecurityContext.Capabilities, "CHOWN")
	//ensureCap(ctr.SecurityContext.Capabilities, "FOWNER")

	// Ensure syncthing container has a /var/syncthing volume mount referencing syncthing-data volume.
	ensureContainerVolumeMount(ctr, "syncthing-data", "/var/syncthing")

	// Ensure syncthing container has STGUIAPIKEY set.
	ensureContainerSTGUIAPIKEY(ctr)
	ensureContainerEnvValue(ctr, "PCAP", "cap_chown,cap_fowner+ep")

	ensureCSIDriverContainer(c, &ds)
	ensureNodeRegistrarContainer(c, &ds)

	if dsExists {
		err = r.Patch(ctx, &ds, dsPatch)
	} else {
		if err := ctrl.SetControllerReference(c, &ds, r.Scheme); err != nil {
			return nil, err
		}
		err = r.Create(ctx, &ds)
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to create/update DaemonSet")
	}

	dss := ds.Status
	if dss.ObservedGeneration < ds.Generation || dss.NumberReady < dss.DesiredNumberScheduled || dss.UpdatedNumberScheduled < dss.DesiredNumberScheduled {
		return nil, r.updateReadiness(ctx, log, c, false, "DaemonSetNotUpToDate", "DaemonSet has not fully rolled out yet")
	}

	return &ds, nil
}

func ensurePodHostPathVolume(pod *corev1.PodSpec, name, path string, t corev1.HostPathType) {
	vol := ensurePodVolume(pod, name)
	if vol.HostPath == nil {
		*vol = corev1.Volume{Name: name}
	}
	vol.HostPath = &corev1.HostPathVolumeSource{
		Path: path,
		Type: &t,
	}
}

func ensurePodContainer(pod *corev1.PodSpec, name string) *corev1.Container {
	for i := range pod.Containers {
		if pod.Containers[i].Name == name {
			return &pod.Containers[i]
		}
	}
	pod.Containers = append(pod.Containers, corev1.Container{Name: name})
	return &pod.Containers[len(pod.Containers)-1]
}

func ensurePodVolume(pod *corev1.PodSpec, name string) *corev1.Volume {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == name {
			return &pod.Volumes[i]
		}
	}
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: name})
	return &pod.Volumes[len(pod.Volumes)-1]
}

func removeContainerEnv(ctr *corev1.Container, name string) {
	for i, e := range ctr.Env {
		if e.Name == name {
			ctr.Env[i] = ctr.Env[len(ctr.Env)-1]
			ctr.Env = ctr.Env[:len(ctr.Env)-1]
			return
		}
	}
}

func ensureContainerEnvValue(ctr *corev1.Container, name, value string) {
	removeContainerEnv(ctr, name)
	ctr.Env = append(ctr.Env, corev1.EnvVar{
		Name:  name,
		Value: value,
	})
}

func ensureContainerEnvValueFrom(ctr *corev1.Container, name string, env *corev1.EnvVarSource) {
	removeContainerEnv(ctr, name)
	ctr.Env = append(ctr.Env, corev1.EnvVar{
		Name:      name,
		ValueFrom: env,
	})
}

func ensureContainerSTGUIAPIKEY(ctr *corev1.Container) {
	ensureContainerEnvValueFrom(ctr, "STGUIAPIKEY", &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
	})
}

func ensureContainerVolumeMount(ctr *corev1.Container, name string, path string) *corev1.VolumeMount {
	for _, v := range ctr.VolumeMounts {
		if v.Name == name {
			if path != "" {
				v.MountPath = path
			}
			return &v
		}
	}
	ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: path})
	return &ctr.VolumeMounts[len(ctr.VolumeMounts)-1]
}

func ensureCSIDriverContainer(c *stc.SyncthingCluster, a *appsv1.DaemonSet) {
	ctr := ensurePodContainer(&a.Spec.Template.Spec, "stc-csi")
	if ctr.Image == "" {
		ctr.Image = os.Getenv("IMAGE")
	}
	if ctr.SecurityContext == nil {
		ctr.SecurityContext = &corev1.SecurityContext{}
	}
	ctr.SecurityContext.Privileged = pointer.Bool(true)
	ensureContainerSTGUIAPIKEY(ctr)
	ensureContainerEnvValueFrom(ctr, "NODE_NAME", &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
	})
	ensureContainerEnvValue(ctr, "ENDPOINT", "127.0.0.1:8384")
	ensureContainerEnvValue(ctr, "DRIVER_NAME", string(c.UID)+".stc.samcday.com")
	ensureContainerEnvValue(ctr, "CSI_SOCKET", "/run/csi/socket")

	var devices []string
	for _, v := range c.Status.Nodes {
		devices = append(devices, v.DeviceID)
	}

	mp := corev1.MountPropagationBidirectional
	mnt := ensureContainerVolumeMount(ctr, "syncthing-data", "/var/syncthing")
	mnt.MountPropagation = &mp
	ensureContainerVolumeMount(ctr, "csi", "/run/csi")
	ensureContainerVolumeMount(ctr, "dev", "/dev")
	mnt = ensureContainerVolumeMount(ctr, "kubelet", "/var/lib/kubelet")
	mnt.MountPropagation = &mp
}

func ensureNodeRegistrarContainer(c *stc.SyncthingCluster, a *appsv1.DaemonSet) {
	ctr := ensurePodContainer(&a.Spec.Template.Spec, "csi-node-driver-registrar")
	if ctr.Image == "" {
		ctr.Image = "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.7.0"
	}
	ctr.Args = append(ctr.Args, fmt.Sprintf("--kubelet-registration-path=/var/lib/kubelet/plugins/%s.stc.samcday.com/socket", c.UID))
	ensureContainerVolumeMount(ctr, "csi", "/run/csi")
	ensureContainerVolumeMount(ctr, "registration", "/registration")
}

func (r *SyncthingClusterReconciler) ensureStorageClass(ctx context.Context, log logr.Logger, c *stc.SyncthingCluster) error {
	var sc storagev1.StorageClass
	className := storageClassName(c)
	driverName := csiDriverName(c)
	err := r.Get(ctx, types.NamespacedName{Name: className}, &sc)
	if err != nil && !k8serr.IsNotFound(err) {
		return err
	}
	exists := sc.Name != ""
	patch := client.MergeFrom(sc.DeepCopy())
	sc.Name = className
	sc.Provisioner = driverName
	vbm := storagev1.VolumeBindingWaitForFirstConsumer
	sc.VolumeBindingMode = &vbm
	if exists {
		return r.Patch(ctx, &sc, patch)
	} else {
		return r.Create(ctx, &sc)
	}
}

func csiDriverName(c *stc.SyncthingCluster) string {
	return fmt.Sprintf("%s.stc.samcday.com", c.UID)
}

func storageClassName(c *stc.SyncthingCluster) string {
	if c.Spec.StorageClassName != "" {
		return c.Spec.StorageClassName
	}
	return fmt.Sprintf("syncthing-%s-%s", c.Namespace, c.Name)
}

func (r *SyncthingClusterReconciler) ensureCSIDriver(ctx context.Context, log logr.Logger, c *stc.SyncthingCluster) error {
	var driver storagev1.CSIDriver
	driverName := csiDriverName(c)
	err := r.Get(ctx, types.NamespacedName{Name: driverName}, &driver)
	if err != nil && !k8serr.IsNotFound(err) {
		return err
	}
	exists := driver.Name != ""
	patch := client.MergeFrom(driver.DeepCopy())
	driver.Name = driverName
	driver.Spec.AttachRequired = pointer.Bool(false)
	driver.Spec.RequiresRepublish = pointer.Bool(true)
	fsgp := storagev1.FileFSGroupPolicy
	driver.Spec.FSGroupPolicy = &fsgp
	driver.Spec.VolumeLifecycleModes = []storagev1.VolumeLifecycleMode{storagev1.VolumeLifecyclePersistent}
	if exists {
		return r.Patch(ctx, &driver, patch)
	}
	return r.Create(ctx, &driver)
}

func (r *SyncthingClusterReconciler) updateReadiness(ctx context.Context, log logr.Logger, c *stc.SyncthingCluster, isReady bool, reason, message string) error {
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

	resp, err := stAPI(client, ip, "stats/device", &deviceResp)
	if err != nil {
		return
	}
	defer resp.Body.Close()
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

	uid := string(c.UID)
	if cfg.GUI.User != "user" || cfg.GUI.CompareHashedPassword(uid) != nil || cfg.GUI.APIKey != uid {
		cfg.GUI.User = "user"
		cfg.GUI.Password = uid
		cfg.GUI.APIKey = uid
		body, err := json.Marshal(cfg.GUI)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("PUT", fmt.Sprintf("http://%s:8384/rest/config/gui", ip), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := stClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("GUI config update HTTP error: %d", resp.StatusCode)
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
