package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
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
	setCondition(cc, portalv1alpha1.ConditionValidated, metav1.ConditionTrue, "ValidationPassed", "all checks passed")
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

	// 2e. Create CAPI objects.
	if err := r.ensureCAPIObjects(ctx, cc, site); err != nil {
		return ctrl.Result{}, err
	}

	setCondition(cc, portalv1alpha1.ConditionMachineCfgDone, metav1.ConditionTrue, "MachineCfgComplete", "Hardware objects created")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, cc)
}

// ── Step 3 : Deploy addons via capi-addon-provider ────────────────────────

func (r *ClusterClaimReconciler) stepWatchAddons(ctx context.Context, cc *portalv1alpha1.ClusterClaim) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("step: ensure addon HelmReleases")

	ap := &portalv1alpha1.AddonProfile{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: cc.Spec.AddonProfile, Namespace: portalSystemNamespace,
	}, ap); err != nil {
		return ctrl.Result{}, err
	}

	components := make([]portalv1alpha1.AddonComponent, len(ap.Spec.Components))
	copy(components, ap.Spec.Components)
	sort.Slice(components, func(i, j int) bool {
		return components[i].Order < components[j].Order
	})

	for _, comp := range components {
		if err := r.ensureHelmRelease(ctx, cc, comp); err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
	}

	// Check readiness of all required components.
	allReady := true
	for _, comp := range components {
		if !comp.Required {
			continue
		}
		ready, err := r.helmReleaseReady(ctx, cc, comp.Name)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		if !ready {
			allReady = false
		}
	}

	if !allReady {
		log.Info("waiting for required HelmReleases to become ready")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseAddonsReady
	setCondition(cc, portalv1alpha1.ConditionAddonsReady, metav1.ConditionTrue, "AllAddonsReady", "all required HelmReleases are ready")
	return ctrl.Result{Requeue: true}, r.Status().Update(ctx, cc)
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
	setCondition(cc, portalv1alpha1.ConditionValidated, metav1.ConditionFalse, reason, msg)
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

// buildRookValues generates inline Helm values for rook-ceph OSD configuration.
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

	tcpName := cc.Name + "-control-plane"
	infraName := cc.Name + "-infra"
	machineTemplateName := cc.Name + "-mt"

	// a. TenantControlPlane (Kamaji) — unstructured.
	tcp := &unstructured.Unstructured{}
	tcp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kamaji.clastix.io",
		Version: "v1alpha1",
		Kind:    "TenantControlPlane",
	})
	tcp.SetName(tcpName)
	tcp.SetNamespace(cc.Namespace)
	if err := ctrl.SetControllerReference(cc, tcp, r.Scheme); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(tcp.Object, map[string]interface{}{
		"endpoint": map[string]interface{}{
			"host": cc.Status.ControlPlaneIP,
			"port": int64(6443),
		},
	}, "spec", "controlPlane"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(tcp.Object, []interface{}{
		map[string]interface{}{"name": "--oidc-issuer-url", "value": site.Spec.OIDC.IssuerURL},
		map[string]interface{}{"name": "--oidc-client-id", "value": site.Spec.OIDC.ClientID},
		map[string]interface{}{"name": "--oidc-username-claim", "value": site.Spec.OIDC.UsernameClaim},
		map[string]interface{}{"name": "--oidc-groups-claim", "value": site.Spec.OIDC.GroupsClaim},
	}, "spec", "addons", "apiServerArguments"); err != nil {
		return err
	}
	if err := r.applyUnstructured(ctx, tcp); err != nil {
		return fmt.Errorf("TenantControlPlane: %w", err)
	}

	// b. TinkerbellCluster (CAPT) — unstructured.
	tbCluster := &unstructured.Unstructured{}
	tbCluster.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "TinkerbellCluster",
	})
	tbCluster.SetName(infraName)
	tbCluster.SetNamespace(cc.Namespace)
	if err := ctrl.SetControllerReference(cc, tbCluster, r.Scheme); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(tbCluster.Object, map[string]interface{}{
		"host": cc.Status.ControlPlaneIP,
		"port": int64(6443),
	}, "spec", "controlPlaneEndpoint"); err != nil {
		return err
	}
	if err := r.applyUnstructured(ctx, tbCluster); err != nil {
		return fmt.Errorf("TinkerbellCluster: %w", err)
	}

	// c. CAPI Cluster (typed).
	tenantSlug := tenantFromNamespace(cc.Namespace)
	capiCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cc.Name,
			Namespace: cc.Namespace,
			Labels: map[string]string{
				labelTenant:       tenantSlug,
				labelSite:         cc.Spec.Site,
				labelAddonProfile: cc.Spec.AddonProfile,
				"cluster-api.cattle.io/rancher-auto-import": "true",
			},
		},
	}
	if err := ctrl.SetControllerReference(cc, capiCluster, r.Scheme); err != nil {
		return err
	}
	_, err := ctrl.CreateOrUpdate(ctx, r.Client, capiCluster, func() error {
		capiCluster.Spec.InfrastructureRef = &corev1.ObjectReference{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
			Kind:       "TinkerbellCluster",
			Name:       infraName,
			Namespace:  cc.Namespace,
		}
		capiCluster.Spec.ControlPlaneRef = &corev1.ObjectReference{
			APIVersion: "kamaji.clastix.io/v1alpha1",
			Kind:       "TenantControlPlane",
			Name:       tcpName,
			Namespace:  cc.Namespace,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("Cluster: %w", err)
	}

	// d. TinkerbellMachineTemplate — unstructured.
	tbMT := &unstructured.Unstructured{}
	tbMT.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "TinkerbellMachineTemplate",
	})
	tbMT.SetName(machineTemplateName)
	tbMT.SetNamespace(cc.Namespace)
	if err := ctrl.SetControllerReference(cc, tbMT, r.Scheme); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(tbMT.Object, map[string]interface{}{}, "spec", "template", "spec"); err != nil {
		return err
	}
	if err := r.applyUnstructured(ctx, tbMT); err != nil {
		return fmt.Errorf("TinkerbellMachineTemplate: %w", err)
	}

	// e. MachineDeployment (CAPI typed).
	replicas := int32(cc.Spec.MachineCount)
	md := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cc.Name + "-md",
			Namespace: cc.Namespace,
		},
	}
	if err := ctrl.SetControllerReference(cc, md, r.Scheme); err != nil {
		return err
	}
	_, err = ctrl.CreateOrUpdate(ctx, r.Client, md, func() error {
		md.Spec.ClusterName = cc.Name
		md.Spec.Replicas = &replicas
		md.Spec.Template.Spec.ClusterName = cc.Name
		md.Spec.Template.Spec.InfrastructureRef = corev1.ObjectReference{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
			Kind:       "TinkerbellMachineTemplate",
			Name:       machineTemplateName,
			Namespace:  cc.Namespace,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("MachineDeployment: %w", err)
	}

	// f. LoadBalancer Service for TenantControlPlane.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tcpName + "-lb",
			Namespace: cc.Namespace,
		},
	}
	if err := ctrl.SetControllerReference(cc, svc, r.Scheme); err != nil {
		return err
	}
	_, err = ctrl.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Annotations = map[string]string{
			"io.cilium/lb-ipam-ips":           cc.Status.ControlPlaneIP,
			"io.cilium/lb-ipam-pool":          site.Spec.Cilium.L2PoolName,
		}
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		svc.Spec.Selector = map[string]string{
			"kamaji.clastix.io/name": tcpName,
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:     "apiserver",
			Port:     6443,
			Protocol: corev1.ProtocolTCP,
		}}
		return nil
	})
	if err != nil {
		return fmt.Errorf("LoadBalancer Service: %w", err)
	}

	// Update ClusterRef.
	if cc.Status.ClusterRef == nil {
		cc.Status.ClusterRef = &portalv1alpha1.LocalObjectRef{}
	}
	cc.Status.ClusterRef.Name = cc.Name

	// Check if CAPI Cluster control plane is ready; caller handles status update.
	existing := &clusterv1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: cc.Name, Namespace: cc.Namespace}, existing); err != nil {
		return err
	}
	if existing.Status.ControlPlaneReady {
		cc.Status.Phase = portalv1alpha1.ClusterClaimPhaseClusterReady
		setCondition(cc, portalv1alpha1.ConditionCAPIReady, metav1.ConditionTrue, "ControlPlaneReady", "CAPI cluster control plane is ready")
	}

	return nil
}

// applyUnstructured creates the object if it does not exist, and skips update if it already does.
func (r *ClusterClaimReconciler) applyUnstructured(ctx context.Context, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	return err
}

// ensureHelmRelease creates or updates a capi-addon-provider HelmRelease for one AddonComponent.
func (r *ClusterClaimReconciler) ensureHelmRelease(ctx context.Context,
	cc *portalv1alpha1.ClusterClaim, comp portalv1alpha1.AddonComponent) error {

	values := comp.HelmRef.Values
	if comp.Name == "rook-ceph" && len(cc.Status.OSDDevices) > 0 {
		values = values + "\n" + buildRookValues(cc.Status.OSDDevices)
	}

	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "addons.stackhpc.com",
		Version: "v1alpha1",
		Kind:    "HelmRelease",
	})
	hr.SetName(cc.Name + "-" + comp.Name)
	hr.SetNamespace(cc.Namespace)
	if err := ctrl.SetControllerReference(cc, hr, r.Scheme); err != nil {
		return err
	}

	bootstrap := comp.Order == 1
	if err := unstructured.SetNestedField(hr.Object, cc.Name, "spec", "clusterName"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(hr.Object, comp.HelmRef.RepoURL, "spec", "chart", "repo"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(hr.Object, comp.HelmRef.ChartName, "spec", "chart", "name"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(hr.Object, comp.HelmRef.ChartVersion, "spec", "chart", "version"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(hr.Object, values, "spec", "values"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(hr.Object, bootstrap, "spec", "bootstrap"); err != nil {
		return err
	}

	return r.applyUnstructured(ctx, hr)
}

// helmReleaseReady returns true when the HelmRelease for the given component reports ready.
func (r *ClusterClaimReconciler) helmReleaseReady(ctx context.Context,
	cc *portalv1alpha1.ClusterClaim, componentName string) (bool, error) {

	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "addons.stackhpc.com",
		Version: "v1alpha1",
		Kind:    "HelmRelease",
	})
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cc.Name + "-" + componentName,
		Namespace: cc.Namespace,
	}, hr); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	ready, _, _ := unstructured.NestedBool(hr.Object, "status", "ready")
	return ready, nil
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
