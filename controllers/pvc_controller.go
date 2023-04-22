package controllers

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	klog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"time"
)

type PVCReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get

func (r *PVCReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := klog.FromContext(ctx)

	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, req.NamespacedName, &pvc)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(client.IgnoreNotFound(err), "failed to get PVC")
	}
	if pvc.Status.Phase != corev1.ClaimPending || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		log.V(3).Info("ignoring PVC - it is not pending")
		return reconcile.Result{}, nil
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		log.V(3).Info("ignoring PVC - it has no storage class name")
		return reconcile.Result{}, nil
	}

	var sc storagev1.StorageClass
	err = r.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, &sc)
	if k8serr.IsNotFound(err) {
		log.V(3).Info("ignoring PVC - storage class not found", "storageClass", pvc.Spec.StorageClassName)
		return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to get StorageClass")
	}
	if !strings.HasSuffix(sc.Provisioner, ".stc.samcday.com") {
		log.V(3).Info("ignoring PVC - it does not have a recognized provisioner")
		return reconcile.Result{}, nil
	}

	clusterUID := strings.TrimSuffix(sc.Provisioner, ".stc.samcday.com")

	var pv corev1.PersistentVolume
	pvName := fmt.Sprintf("pvc-%s", pvc.UID)
	err = r.Get(ctx, types.NamespacedName{Name: pvName}, &pv)
	if err != nil && !k8serr.IsNotFound(err) {
		return reconcile.Result{}, errors.Wrap(err, "failed to get PV")
	}
	if pv.Name == "" {
		log.Info("creating PV")
		pv.Name = pvName
		pv.Spec.AccessModes = pvc.Spec.AccessModes
		pv.Spec.Capacity = pvc.Spec.Resources.Requests
		pv.Spec.PersistentVolumeReclaimPolicy = *sc.ReclaimPolicy
		pv.Spec.StorageClassName = sc.Name
		vm := corev1.PersistentVolumeFilesystem
		pv.Spec.VolumeMode = &vm
		pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{
			Driver:       sc.Provisioner,
			VolumeHandle: pvName,
		}
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{
			Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "cluster.stc.samcday.com/" + clusterUID,
						Operator: corev1.NodeSelectorOpExists,
					}},
				}},
			},
		}
		err := r.Create(ctx, &pv)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to create PV")
		}
	}
	if pvc.Spec.VolumeName != pvName {
		log.Info("patching PVC spec.volumeName")
		patch := client.MergeFrom(pvc.DeepCopy())
		pvc.Spec.VolumeName = pvName
		err := r.Patch(ctx, &pvc, patch)
		if err != nil {
			return reconcile.Result{}, errors.Wrap(err, "failed to patch PVC")
		}
	}
	return reconcile.Result{}, nil
}

func (r *PVCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
