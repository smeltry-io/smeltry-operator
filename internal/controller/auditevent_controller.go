// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	portalv1alpha1 "github.com/smeltry-io/smeltry-operator/api/v1alpha1"
)

// AuditEventPurgeReconciler deletes AuditEvent objects whose TTL has elapsed.
type AuditEventPurgeReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	DefaultTTL time.Duration
}

// +kubebuilder:rbac:groups=portal.smeltry.io,resources=auditevents,verbs=get;list;watch;create;delete

func (r *AuditEventPurgeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	ev := &portalv1alpha1.AuditEvent{}
	if err := r.Get(ctx, req.NamespacedName, ev); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	ttl := r.DefaultTTL
	if ev.Spec.TTL != "" {
		parsed, err := time.ParseDuration(ev.Spec.TTL)
		if err != nil {
			log.Error(err, "invalid TTL; using default", "ttl", ev.Spec.TTL)
		} else {
			ttl = parsed
		}
	}

	expiry := ev.CreationTimestamp.Time.Add(ttl)
	if time.Now().Before(expiry) {
		return ctrl.Result{RequeueAfter: time.Until(expiry) + time.Second}, nil
	}

	log.Info("deleting expired AuditEvent", "name", ev.Name, "namespace", ev.Namespace)
	if err := r.Delete(ctx, ev); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *AuditEventPurgeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&portalv1alpha1.AuditEvent{}).
		Complete(r)
}

// ── AuditEvent emission helpers ───────────────────────────────────────────────

// emitAuditEvent creates an AuditEvent in the given namespace. Errors are
// logged but do not fail the reconciliation — audit events are best-effort.
func emitAuditEvent(ctx context.Context, c client.Client, namespace, defaultTTL string, spec portalv1alpha1.AuditEventSpec) {
	log := log.FromContext(ctx)

	spec.Timestamp = metav1.Now()
	if spec.TTL == "" {
		spec.TTL = defaultTTL
	}
	if spec.Actor == "" {
		spec.Actor = "smeltry-operator"
	}

	ev := &portalv1alpha1.AuditEvent{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-", spec.ResourceName, spec.Type),
			Namespace:    namespace,
		},
		Spec: spec,
	}
	if err := c.Create(ctx, ev); err != nil {
		log.Error(err, "failed to emit AuditEvent", "type", spec.Type, "resource", spec.ResourceName)
	}
}
