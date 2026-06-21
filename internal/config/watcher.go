// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package config

import (
	"context"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/smeltry-io/smeltry-operator/internal/netbox"
)

// ConfigReconciler watches one ConfigMap and one Secret.
// When either changes, it rebuilds the Netbox client and updates the holder.
type ConfigReconciler struct {
	client.Client
	Holder        *NetboxHolder
	ConfigMapName string
	SecretName    string
	Namespace     string
}

// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func (r *ConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	slog.InfoContext(ctx, "config reconcile triggered", "object", req.NamespacedName)

	url, token, err := r.readConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to read config, keeping previous client", "err", err)
		return ctrl.Result{}, nil
	}

	if url == "" || token == "" {
		slog.WarnContext(ctx, "netbox.url or netbox.token is empty, skipping client update")
		return ctrl.Result{}, nil
	}

	r.Holder.Set(netbox.NewClient(url, token))
	slog.InfoContext(ctx, "netbox client refreshed", "url", url)
	return ctrl.Result{}, nil
}

// readConfig reads netbox.url from the ConfigMap and netbox.token from the Secret.
func (r *ConfigReconciler) readConfig(ctx context.Context) (url, token string, err error) {
	cm := &corev1.ConfigMap{}
	if err = r.Get(ctx, types.NamespacedName{Name: r.ConfigMapName, Namespace: r.Namespace}, cm); err != nil {
		return
	}
	url = cm.Data["netbox.url"]

	secret := &corev1.Secret{}
	if err = r.Get(ctx, types.NamespacedName{Name: r.SecretName, Namespace: r.Namespace}, secret); err != nil {
		return
	}
	token = string(secret.Data["netbox.token"])
	return
}

// nameFilter returns a predicate that only passes objects with the given name.
func nameFilter(name string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return e.Object.GetName() == name },
		UpdateFunc:  func(e event.UpdateEvent) bool { return e.ObjectNew.GetName() == name },
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

// enqueueFixed always enqueues the same fixed request (the ConfigMap).
func (r *ConfigReconciler) enqueueFixed() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: r.ConfigMapName, Namespace: r.Namespace}},
		}
	})
}

func (r *ConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(nameFilter(r.ConfigMapName))).
		Watches(&corev1.Secret{},
			r.enqueueFixed(),
			builder.WithPredicates(nameFilter(r.SecretName)),
		).
		Named("config").
		Complete(r)
}
