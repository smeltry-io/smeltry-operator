package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

// NetboxTenantReconciler polls Netbox tenants and ensures a matching
// Kubernetes namespace + ResourceQuota + RoleBinding exist for each one.
//
// There is no CRD for this controller: it is driven by time-based requeue
// (or a Netbox webhook endpoint hitting a dedicated HTTP handler).
type NetboxTenantReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	NetboxClient *netbox.Client
	// PollInterval controls how frequently Netbox is queried.
	// Default: 5 minutes.
	PollInterval time.Duration
}

// Reconcile is called periodically. req is a sentinel object; the real
// reconciliation iterates over all Netbox tenants.
func (r *NetboxTenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("syncing Netbox tenants")

	tenants, err := r.NetboxClient.ListTenants(ctx)
	if err != nil {
		log.Error(err, "failed to list Netbox tenants")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	for _, t := range tenants {
		if err := r.reconcileTenant(ctx, t); err != nil {
			log.Error(err, "failed to reconcile tenant", "slug", t.Slug)
			// Continue with the rest; individual errors are logged.
		}
	}

	interval := r.PollInterval
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *NetboxTenantReconciler) reconcileTenant(ctx context.Context, t netbox.Tenant) error {
	nsName := "tenant-" + t.Slug

	// ── Namespace ──────────────────────────────────────────────────────────
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns)
	if errors.IsNotFound(err) {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: nsName,
				Labels: map[string]string{
					"portal.smeltry.io/tenant": t.Slug,
				},
			},
		}
		if err := r.Create(ctx, ns); err != nil {
			return fmt.Errorf("create namespace %s: %w", nsName, err)
		}
	} else if err != nil {
		return err
	}

	// ── ResourceQuota ──────────────────────────────────────────────────────
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "smeltry-quota",
			Namespace: nsName,
		},
	}
	_, err = ctrl.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Spec = corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				// Custom resources are counted by the API server as
				// count/<resource>.<group>.
				corev1.ResourceName("count/clusterclaims.portal.smeltry.io"): resource.MustParse(
					fmt.Sprint(maxInt(t.CustomFields.K8sMaxClusters, 1))),
				corev1.ResourceName("count/serverclaims.portal.smeltry.io"): resource.MustParse("10"),
			},
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert quota in %s: %w", nsName, err)
	}

	// ── Role ───────────────────────────────────────────────────────────────
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-user",
			Namespace: nsName,
		},
	}
	_, err = ctrl.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{"portal.smeltry.io"},
				Resources: []string{"clusterclaims", "serverclaims"},
				Verbs:     []string{"get", "list", "create", "delete"},
			},
			{
				// resourceNames is patched by ClusterClaimReconciler when
				// a cluster reaches Ready.
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				Verbs:         []string{"get"},
				ResourceNames: []string{},
			},
			{
				// Read-only access to AddonProfiles in portal-system.
				// Requires a ClusterRole binding at the cluster level.
				APIGroups: []string{"portal.smeltry.io"},
				Resources: []string{"addonprofiles", "siteconfigs"},
				Verbs:     []string{"get", "list"},
			},
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert role in %s: %w", nsName, err)
	}

	// ── RoleBinding ────────────────────────────────────────────────────────
	// Binds the Authentik group (same slug as the Netbox tenant) to the role.
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-user-binding",
			Namespace: nsName,
		},
	}
	_, err = ctrl.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Subjects = []rbacv1.Subject{{
			Kind:     "Group",
			Name:     t.Slug, // matches Authentik group slug by convention
			APIGroup: "rbac.authorization.k8s.io",
		}}
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "cluster-user",
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert rolebinding in %s: %w", nsName, err)
	}

	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SetupWithManager registers the controller. It uses a channel-based trigger
// rather than watching a CRD — reconciliation is time-driven.
func (r *NetboxTenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A sentinel ConfigMap in portal-system acts as the reconcile trigger.
	// The controller requeues itself on every reconcile via RequeueAfter.
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Complete(r)
}
