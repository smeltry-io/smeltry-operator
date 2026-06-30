// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/config"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

const serverClaimFinalizer = "portal.smeltry.io/serverclaim-protection"

// ServerClaimReconciler reconciles ServerClaim objects.
type ServerClaimReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	NetboxHolder    *config.NetboxHolder
	NetboxToken     string
	NetboxURL       string
	MachinecfgImage string
	DefaultAuditTTL string
}

// +kubebuilder:rbac:groups=portal.smeltry.io,resources=serverclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=portal.smeltry.io,resources=serverclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=portal.smeltry.io,resources=serverclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=portal.smeltry.io,resources=siteconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *ServerClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	sc := &portalv1alpha1.ServerClaim{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sc)
	}

	if !controllerutil.ContainsFinalizer(sc, serverClaimFinalizer) {
		controllerutil.AddFinalizer(sc, serverClaimFinalizer)
		return ctrl.Result{}, r.Update(ctx, sc)
	}

	log.Info("reconciling ServerClaim", "phase", sc.Status.Phase)

	switch sc.Status.Phase {
	case "", portalv1alpha1.ServerClaimPhasePending:
		return r.stepValidate(ctx, sc)
	case portalv1alpha1.ServerClaimPhaseProvisioning:
		return r.stepProvision(ctx, sc)
	case portalv1alpha1.ServerClaimPhaseReady, portalv1alpha1.ServerClaimPhaseFailed:
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, nil
}

// ── Step 1 : Validation ────────────────────────────────────────────────────

func (r *ServerClaimReconciler) stepValidate(ctx context.Context, sc *portalv1alpha1.ServerClaim) (ctrl.Result, error) {
	site := &portalv1alpha1.SiteConfig{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      sc.Spec.Site,
		Namespace: portalSystemNamespace,
	}, site); err != nil {
		return r.failSC(ctx, sc, "SiteConfigNotFound",
			fmt.Sprintf("SiteConfig %q not found in portal-system", sc.Spec.Site))
	}

	available, err := r.NetboxHolder.Get().ListAvailableDevices(ctx,
		site.Spec.Netbox.SiteSlug, sc.Spec.MachineClass)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	if len(available) == 0 {
		return r.failSC(ctx, sc, "InsufficientMachines",
			fmt.Sprintf("no machine of class %q available on site %q", sc.Spec.MachineClass, sc.Spec.Site))
	}

	oldPhase := string(sc.Status.Phase)
	sc.Status.Phase = portalv1alpha1.ServerClaimPhaseProvisioning
	if err := r.Status().Update(ctx, sc); err != nil {
		return ctrl.Result{}, err
	}
	emitAuditEvent(ctx, r.Client, sc.Namespace, r.DefaultAuditTTL, portalv1alpha1.AuditEventSpec{
		Type:         portalv1alpha1.AuditTypePhaseChanged,
		ResourceKind: "ServerClaim",
		ResourceName: sc.Name,
		OldPhase:     oldPhase,
		NewPhase:     string(portalv1alpha1.ServerClaimPhaseProvisioning),
	})
	return ctrl.Result{Requeue: true}, nil
}

// ── Step 2 : Provision (IP → machine → machinecfg Job → Ready) ───────────

func (r *ServerClaimReconciler) stepProvision(ctx context.Context, sc *portalv1alpha1.ServerClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	site := &portalv1alpha1.SiteConfig{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      sc.Spec.Site,
		Namespace: portalSystemNamespace,
	}, site); err != nil {
		return ctrl.Result{}, err
	}

	// 2a. Reserve IP (idempotent).
	if sc.Status.ServerIP == "" {
		log.Info("allocating server IP")
		tenantSlug := tenantFromNamespace(sc.Namespace)
		dnsName := fmt.Sprintf("%s.%s.%s", sc.Name, tenantSlug, site.Spec.DNS.Zone)
		ip, err := r.NetboxHolder.Get().AllocateIP(ctx,
			site.Spec.Netbox.ProvisioningPrefix, dnsName, site.Spec.Netbox.IPAMTags)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		sc.Status.ServerIP = stripPrefix(ip.Address)
		sc.Status.ServerDNS = dnsName
		sc.Status.NetboxIPAMID = ip.ID
		if err := r.Status().Update(ctx, sc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 2b. Allocate machine (idempotent).
	if sc.Status.AllocatedMachineID == 0 {
		log.Info("allocating machine in Netbox")
		available, err := r.NetboxHolder.Get().ListAvailableDevices(ctx,
			site.Spec.Netbox.SiteSlug, sc.Spec.MachineClass)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		if len(available) == 0 {
			return r.failSC(ctx, sc, "InsufficientMachines", "no machine available at provisioning time")
		}
		m := available[0]
		tenantSlug := tenantFromNamespace(sc.Namespace)
		if err := r.NetboxHolder.Get().SetDeviceStatus(ctx, m.ID, netbox.DeviceStatusStaged, tenantSlug); err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		sc.Status.AllocatedMachineID = m.ID
		if err := r.Status().Update(ctx, sc); err != nil {
			return ctrl.Result{}, err
		}
		emitAuditEvent(ctx, r.Client, sc.Namespace, r.DefaultAuditTTL, portalv1alpha1.AuditEventSpec{
			Type:         portalv1alpha1.AuditTypeMachineAllocated,
			ResourceKind: "ServerClaim",
			ResourceName: sc.Name,
			MachineID:    m.ID,
		})
	}

	// 2c. Create machinecfg Job (idempotent).
	jobName := sc.Name + "-machinecfg"
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: sc.Namespace}, job)
	if errors.IsNotFound(err) {
		log.Info("creating machinecfg Job")
		if err := r.createServerJob(ctx, sc, site, jobName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// 2d. Wait for Job completion.
	if job.Status.Failed > 0 {
		return r.failSC(ctx, sc, "MachineCfgFailed", "machinecfg Job failed; check Job logs")
	}
	if job.Status.CompletionTime == nil {
		log.Info("waiting for machinecfg Job")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	sc.Status.Phase = portalv1alpha1.ServerClaimPhaseReady
	return ctrl.Result{}, r.Status().Update(ctx, sc)
}

// ── Deletion finalizer ─────────────────────────────────────────────────────

func (r *ServerClaimReconciler) reconcileDelete(ctx context.Context, sc *portalv1alpha1.ServerClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("running finalizer: releasing resources")

	if sc.Status.NetboxIPAMID != 0 {
		if err := r.NetboxHolder.Get().ReleaseIP(ctx, sc.Status.NetboxIPAMID); err != nil {
			log.Error(err, "failed to release IP", "id", sc.Status.NetboxIPAMID)
		}
	}

	if sc.Status.AllocatedMachineID != 0 {
		if err := r.NetboxHolder.Get().SetDeviceStatus(ctx, sc.Status.AllocatedMachineID,
			netbox.DeviceStatusActive, ""); err != nil {
			log.Error(err, "failed to release machine", "id", sc.Status.AllocatedMachineID)
		}
	}

	emitAuditEvent(ctx, r.Client, sc.Namespace, r.DefaultAuditTTL, portalv1alpha1.AuditEventSpec{
		Type:         portalv1alpha1.AuditTypeServerDeleted,
		ResourceKind: "ServerClaim",
		ResourceName: sc.Name,
	})
	controllerutil.RemoveFinalizer(sc, serverClaimFinalizer)
	return ctrl.Result{}, r.Update(ctx, sc)
}

// ── Helpers ────────────────────────────────────────────────────────────────

func (r *ServerClaimReconciler) failSC(ctx context.Context, sc *portalv1alpha1.ServerClaim, reason, msg string) (ctrl.Result, error) {
	log.FromContext(ctx).Info("ServerClaim failed", "reason", reason)
	sc.Status.Phase = portalv1alpha1.ServerClaimPhaseFailed
	sc.Status.Conditions = append(sc.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
	return ctrl.Result{}, r.Status().Update(ctx, sc)
}

func (r *ServerClaimReconciler) createServerJob(ctx context.Context,
	sc *portalv1alpha1.ServerClaim, site *portalv1alpha1.SiteConfig, jobName string) error {

	tenantSlug := tenantFromNamespace(sc.Namespace)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: sc.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:  "machinecfg",
						Image: r.MachinecfgImage,
						Args: []string{
							"--netbox-endpoint", r.NetboxURL,
							"--netbox-token", r.NetboxToken,
							"--sites", site.Spec.Netbox.SiteSlug,
							"tinkerbell", "hardware",
							"--os", sc.Spec.OS,
						},
						Env: []corev1.EnvVar{
							{Name: "NETBOX_TENANT", Value: tenantSlug},
							{Name: "NETBOX_MODEL", Value: sc.Spec.MachineClass},
							{Name: "NAMESPACE", Value: sc.Namespace},
						},
					}},
				},
			},
		},
	}
	_ = controllerutil.SetControllerReference(sc, job, r.Scheme)
	return r.Create(ctx, job)
}

// SetupWithManager registers the controller with the manager.
func (r *ServerClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&portalv1alpha1.ServerClaim{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
