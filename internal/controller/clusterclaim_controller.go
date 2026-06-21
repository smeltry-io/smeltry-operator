package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

const (
	clusterClaimFinalizer   = "portal.smeltry.io/clusterclaim-protection"
	portalSystemNamespace   = "portal-system"
	machinecfgImage         = "ghcr.io/smeltry-io/machinecfg:latest"
	labelAddonProfile       = "portal.smeltry.io/addon-profile"
	labelTenant             = "portal.smeltry.io/tenant"
	labelSite               = "portal.smeltry.io/site"
)

// ClusterClaimReconciler reconciles ClusterClaim objects.
type ClusterClaimReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	NetboxClient  *netbox.Client
	NetboxToken   string
	NetboxURL     string
}

// +kubebuilder:rbac:groups=portal.smeltry.io,resources=clusterclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=portal.smeltry.io,resources=clusterclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=portal.smeltry.io,resources=clusterclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=portal.smeltry.io,resources=addonprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=portal.smeltry.io,resources=siteconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps;secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;update;patch

func (r *ClusterClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// ── Fetch the ClusterClaim ─────────────────────────────────────────────
	cc := &portalv1alpha1.ClusterClaim{}
	if err := r.Get(ctx, req.NamespacedName, cc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// ── Deletion: run finalizer ────────────────────────────────────────────
	if !cc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cc)
	}

	// ── Ensure finalizer is present ────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(cc, clusterClaimFinalizer) {
		controllerutil.AddFinalizer(cc, clusterClaimFinalizer)
		return ctrl.Result{}, r.Update(ctx, cc)
	}

	log.Info("reconciling", "phase", cc.Status.Phase)

	// ── State machine ──────────────────────────────────────────────────────
	switch cc.Status.Phase {
	case "", portalv1alpha1.ClusterClaimPhasePending:
		return r.stepValidate(ctx, cc)
	case portalv1alpha1.ClusterClaimPhaseProvisioning:
		return r.stepProvision(ctx, cc)
	case portalv1alpha1.ClusterClaimPhaseClusterReady:
		return r.stepWatchAddons(ctx, cc)
	case portalv1alpha1.ClusterClaimPhaseAddonsReady:
		return r.stepExposeKubeconfig(ctx, cc)
	case portalv1alpha1.ClusterClaimPhaseReady:
		// Nothing to do — watch for external changes (CAPI cluster health).
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	case portalv1alpha1.ClusterClaimPhaseFailed:
		// Terminal state; user must delete and recreate.
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// ── Step 1 : Validation ────────────────────────────────────────────────────

func (r *ClusterClaimReconciler) stepValidate(ctx context.Context, cc *portalv1alpha1.ClusterClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("step: validate")

	// 1a. AddonProfile exists in portal-system?
	ap := &portalv1alpha1.AddonProfile{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cc.Spec.AddonProfile,
		Namespace: portalSystemNamespace,
	}, ap); err != nil {
		return r.fail(ctx, cc, "AddonProfileNotFound",
			fmt.Sprintf("AddonProfile %q not found in portal-system", cc.Spec.AddonProfile))
	}

	// 1b. SiteConfig exists in portal-system?
	site := &portalv1alpha1.SiteConfig{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cc.Spec.Site,
		Namespace: portalSystemNamespace,
	}, site); err != nil {
		return r.fail(ctx, cc, "SiteConfigNotFound",
			fmt.Sprintf("SiteConfig %q not found in portal-system", cc.Spec.Site))
	}

	// 1c. Enough machines available in Netbox?
	available, err := r.NetboxClient.ListAvailableDevices(ctx,
		site.Spec.Netbox.SiteSlug, cc.Spec.MachineClass)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	if len(available) < cc.Spec.MachineCount {
		return r.fail(ctx, cc, "InsufficientMachines",
			fmt.Sprintf("need %d machines of class %q, only %d available on site %q",
				cc.Spec.MachineCount, cc.Spec.MachineClass, len(available), cc.Spec.Site))
	}

	// 1d. MachineClass compatible with AddonProfile constraints?
	if ap.Spec.MachineConstraints != nil {
		for _, requiredTag := range ap.Spec.MachineConstraints.RequiredTags {
			if !machinesHaveTag(available[:cc.Spec.MachineCount], requiredTag) {
				return r.fail(ctx, cc, "MachineClassIncompatible",
					fmt.Sprintf("AddonProfile %q requires tag %q on machines", cc.Spec.AddonProfile, requiredTag))
			}
		}
	}

	// Validation passed → move to Provisioning.
	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseProvisioning
	setCondition(cc, ConditionValidated, metav1.ConditionTrue, "ValidationPassed", "all checks passed")
	return ctrl.Result{Requeue: true}, r.Status().Update(ctx, cc)
}

// ── Step 2 : Provision (IPs → machines → machinecfg Job → CAPI objects) ──

func (r *ClusterClaimReconciler) stepProvision(ctx context.Context, cc *portalv1alpha1.ClusterClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	site := &portalv1alpha1.SiteConfig{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: cc.Spec.Site, Namespace: portalSystemNamespace,
	}, site); err != nil {
		return ctrl.Result{}, err
	}

	// 2a. Reserve control plane IP (idempotent: skip if already done).
	if cc.Status.ControlPlaneIP == "" {
		log.Info("step: allocate control plane IP")
		tenantSlug := tenantFromNamespace(cc.Namespace)
		cpDNS := fmt.Sprintf("%s-api.%s.%s", cc.Name, tenantSlug, site.Spec.DNS.Zone)
		ip, err := r.NetboxClient.AllocateIP(ctx,
			site.Spec.Netbox.ProvisioningPrefix, cpDNS, site.Spec.Netbox.IPAMTags)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		cc.Status.ControlPlaneIP = stripPrefix(ip.Address)
		cc.Status.ControlPlaneDNS = cpDNS
		cc.Status.NetboxIPAMIDs = append(cc.Status.NetboxIPAMIDs, ip.ID)
		if err := r.Status().Update(ctx, cc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 2b. Reserve webhook IP.
	if cc.Status.WebhookIP == "" {
		log.Info("step: allocate webhook IP")
		tenantSlug := tenantFromNamespace(cc.Namespace)
		whDNS := fmt.Sprintf("%s-wh.%s.%s", cc.Name, tenantSlug, site.Spec.DNS.Zone)
		ip, err := r.NetboxClient.AllocateIP(ctx,
			site.Spec.Netbox.ProvisioningPrefix, whDNS, site.Spec.Netbox.IPAMTags)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		cc.Status.WebhookIP = stripPrefix(ip.Address)
		cc.Status.WebhookDNS = whDNS
		cc.Status.NetboxIPAMIDs = append(cc.Status.NetboxIPAMIDs, ip.ID)
		if err := r.Status().Update(ctx, cc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 2c. Allocate machines in Netbox (set status=staged).
	if len(cc.Status.AllocatedMachineIDs) == 0 {
		log.Info("step: allocate machines in Netbox")
		machines, err := r.NetboxClient.ListAvailableDevices(ctx,
			site.Spec.Netbox.SiteSlug, cc.Spec.MachineClass)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		tenantSlug := tenantFromNamespace(cc.Namespace)
		osdDevices := make(map[string][]string)

		for i := 0; i < cc.Spec.MachineCount && i < len(machines); i++ {
			m := machines[i]
			if err := r.NetboxClient.SetDeviceStatus(ctx, m.ID, netbox.DeviceStatusStaged, tenantSlug); err != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, err
			}
			cc.Status.AllocatedMachineIDs = append(cc.Status.AllocatedMachineIDs, m.ID)

			// Collect OSD disks from inventory items.
			disks, err := r.NetboxClient.ListOSDDisks(ctx, m.ID)
			if err != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, err
			}
			if len(disks) > 0 {
				key := fmt.Sprint(m.ID)
				for _, d := range disks {
					osdDevices[key] = append(osdDevices[key], d.Name)
				}
			}
		}
		cc.Status.OSDDevices = osdDevices
		if err := r.Status().Update(ctx, cc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 2d. Create machinecfg Job (idempotent: skip if already exists).
	jobName := cc.Name + "-machinecfg"
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: cc.Namespace}, job)
	if errors.IsNotFound(err) {
		log.Info("step: create machinecfg Job")
		if err := r.createMachinecfgJob(ctx, cc, site, jobName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Wait for Job completion.
	if job.Status.CompletionTime == nil {
		log.Info("waiting for machinecfg Job to complete")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if job.Status.Failed > 0 {
		return r.fail(ctx, cc, "MachineCfgFailed", "machinecfg Job failed; check Job logs")
	}

	// 2e. Create Rook ConfigMap.
	if err := r.ensureRookConfigMap(ctx, cc); err != nil {
		return ctrl.Result{}, err
	}

	// 2f. Create CAPI objects.
	if err := r.ensureCAPIObjects(ctx, cc, site); err != nil {
		return ctrl.Result{}, err
	}

	setCondition(cc, ConditionMachineCfgDone, metav1.ConditionTrue, "MachineCfgComplete", "Hardware objects created")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, cc)
}

// ── Step 3 : Watch CAPI + Sveltos ─────────────────────────────────────────

func (r *ClusterClaimReconciler) stepWatchAddons(ctx context.Context, cc *portalv1alpha1.ClusterClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("step: watch Sveltos addons")

	// TODO: query Sveltos ClusterSummary for the tenant cluster.
	// When the ClusterProfile matching cc.Spec.AddonProfile reports Applied:
	//   cc.Status.Phase = AddonsReady
	//   setCondition(cc, ConditionAddonsReady, ...)
	//   return ctrl.Result{Requeue: true}, r.Status().Update(ctx, cc)

	// Placeholder: requeue and check again.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ── Step 4 : Expose kubeconfig ────────────────────────────────────────────

func (r *ClusterClaimReconciler) stepExposeKubeconfig(ctx context.Context, cc *portalv1alpha1.ClusterClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("step: expose kubeconfig")

	secretName := cc.Name + "-kubeconfig"

	// Patch the tenant Role to grant read access to this specific secret.
	role := &rbacv1.Role{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: "cluster-user", Namespace: cc.Namespace,
	}, role); err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, client.IgnoreNotFound(err)
	}
	for i, rule := range role.Rules {
		if rulesCoversSecrets(rule) {
			if !containsString(rule.ResourceNames, secretName) {
				role.Rules[i].ResourceNames = append(role.Rules[i].ResourceNames, secretName)
				if err := r.Update(ctx, role); err != nil {
					return ctrl.Result{}, err
				}
			}
			break
		}
	}

	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseReady
	cc.Status.KubeconfigSecret = secretName
	return ctrl.Result{}, r.Status().Update(ctx, cc)
}

// ── Deletion finalizer ─────────────────────────────────────────────────────

func (r *ClusterClaimReconciler) reconcileDelete(ctx context.Context, cc *portalv1alpha1.ClusterClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("running finalizer: releasing resources")

	// Release Netbox IPAM IPs.
	for _, id := range cc.Status.NetboxIPAMIDs {
		if err := r.NetboxClient.ReleaseIP(ctx, id); err != nil {
			log.Error(err, "failed to release Netbox IP", "id", id)
		}
	}

	// Release machines (set status back to active).
	for _, id := range cc.Status.AllocatedMachineIDs {
		if err := r.NetboxClient.SetDeviceStatus(ctx, id, netbox.DeviceStatusActive, ""); err != nil {
			log.Error(err, "failed to release machine", "id", id)
		}
	}

	// Remove kubeconfig from tenant Role resourceNames.
	if cc.Status.KubeconfigSecret != "" {
		role := &rbacv1.Role{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: "cluster-user", Namespace: cc.Namespace,
		}, role); err == nil {
			for i, rule := range role.Rules {
				if rulesCoversSecrets(rule) {
					role.Rules[i].ResourceNames = removeString(rule.ResourceNames, cc.Status.KubeconfigSecret)
				}
			}
			_ = r.Update(ctx, role)
		}
	}

	// CAPI objects are deleted by ownerReference cascade.
	// machinecfg will delete Hardware objects when machines leave staged status.

	controllerutil.RemoveFinalizer(cc, clusterClaimFinalizer)
	return ctrl.Result{}, r.Update(ctx, cc)
}

// ── Helpers ────────────────────────────────────────────────────────────────

func (r *ClusterClaimReconciler) fail(ctx context.Context, cc *portalv1alpha1.ClusterClaim, reason, msg string) (ctrl.Result, error) {
	log.FromContext(ctx).Info("ClusterClaim failed", "reason", reason, "msg", msg)
	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseFailed
	setCondition(cc, ConditionValidated, metav1.ConditionFalse, reason, msg)
	return ctrl.Result{}, r.Status().Update(ctx, cc)
}

func (r *ClusterClaimReconciler) createMachinecfgJob(ctx context.Context,
	cc *portalv1alpha1.ClusterClaim, site *portalv1alpha1.SiteConfig, jobName string) error {

	tenantSlug := tenantFromNamespace(cc.Namespace)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: cc.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:  "machinecfg",
						Image: machinecfgImage,
						Args: []string{
							"--netbox-endpoint", r.NetboxURL,
							"--netbox-token", r.NetboxToken,
							"--sites", site.Spec.Netbox.SiteSlug,
							"tinkerbell", "hardware",
							"--embed-ignition-as-vendor-data",
							"--embedded-ignition-variant=flatcar",
						},
						Env: []corev1.EnvVar{
							{Name: "NETBOX_TENANT", Value: tenantSlug},
							{Name: "NETBOX_MODEL", Value: cc.Spec.MachineClass},
							{Name: "NAMESPACE", Value: cc.Namespace},
						},
					}},
				},
			},
		},
	}
	_ = controllerutil.SetControllerReference(cc, job, r.Scheme)
	return r.Create(ctx, job)
}

func (r *ClusterClaimReconciler) ensureRookConfigMap(ctx context.Context, cc *portalv1alpha1.ClusterClaim) error {
	if len(cc.Status.OSDDevices) == 0 {
		return nil
	}
	cmName := cc.Name + "-rook-config"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: cc.Namespace,
		},
	}
	_, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{
			"values.yaml": buildRookValues(cc.Status.OSDDevices),
		}
		return controllerutil.SetControllerReference(cc, cm, r.Scheme)
	})
	return err
}

func buildRookValues(osdDevices map[string][]string) string {
	var sb strings.Builder
	sb.WriteString("ceph:\n  storage:\n    nodes:\n")
	for nodeID, disks := range osdDevices {
		sb.WriteString(fmt.Sprintf("    - name: \"worker-%s\"\n      devices:\n", nodeID))
		for _, d := range disks {
			sb.WriteString(fmt.Sprintf("      - name: %s\n", d))
		}
	}
	return sb.String()
}

func (r *ClusterClaimReconciler) ensureCAPIObjects(ctx context.Context,
	cc *portalv1alpha1.ClusterClaim, site *portalv1alpha1.SiteConfig) error {
	// TODO: create CAPI Cluster + TinkerbellCluster + TenantControlPlane (Kamaji)
	// + TinkerbellMachineTemplate + MachineDeployment.
	//
	// Key values from status:
	//   controlPlaneIP    → TenantControlPlane.spec.controlPlane.endpoint.host
	//   site.Spec.Cilium.L2PoolName → annotation on control plane Service
	//
	// Labels to set on Cluster:
	//   portal.smeltry.io/tenant        = tenantFromNamespace(cc.Namespace)
	//   portal.smeltry.io/site          = cc.Spec.Site
	//   portal.smeltry.io/addon-profile = cc.Spec.AddonProfile
	//
	// This drives Sveltos ClusterProfile selection automatically.
	return nil
}

// ── Condition helpers ──────────────────────────────────────────────────────

func setCondition(cc *portalv1alpha1.ClusterClaim, condType string,
	status metav1.ConditionStatus, reason, msg string) {
	now := metav1.Now()
	for i, c := range cc.Status.Conditions {
		if c.Type == condType {
			cc.Status.Conditions[i].Status = status
			cc.Status.Conditions[i].Reason = reason
			cc.Status.Conditions[i].Message = msg
			cc.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	cc.Status.Conditions = append(cc.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
}

// ── Utility functions ──────────────────────────────────────────────────────

// tenantFromNamespace extracts the tenant slug from a namespace of the form "tenant-<slug>".
func tenantFromNamespace(ns string) string {
	return strings.TrimPrefix(ns, "tenant-")
}

// stripPrefix removes the /prefix-length suffix from a CIDR notation IP.
func stripPrefix(cidr string) string {
	if i := strings.IndexByte(cidr, '/'); i >= 0 {
		return cidr[:i]
	}
	return cidr
}

func machinesHaveTag(machines []netbox.Device, tag string) bool {
	for _, m := range machines {
		for _, t := range m.Tags {
			if t.Slug == tag {
				return true
			}
		}
	}
	return false
}

func rulesCoversSecrets(rule rbacv1.PolicyRule) bool {
	for _, r := range rule.Resources {
		if r == "secrets" {
			return true
		}
	}
	return false
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	out := slice[:0]
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// SetupWithManager registers the controller with the manager.
func (r *ClusterClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&portalv1alpha1.ClusterClaim{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
