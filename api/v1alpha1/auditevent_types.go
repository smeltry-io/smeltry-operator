// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The Smeltry Authors

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AuditEvent types emitted by smeltry-operator.
const (
	AuditTypePhaseChanged    = "PhaseChanged"
	AuditTypeMachineAllocated = "MachineAllocated"
	AuditTypeIPAllocated     = "IPAllocated"
	AuditTypeClusterDeleted  = "ClusterDeleted"
	AuditTypeServerDeleted   = "ServerDeleted"
)

// AuditEventSpec describes one auditable action performed by smeltry-operator.
type AuditEventSpec struct {
	// Type is the category of the event (e.g. PhaseChanged, MachineAllocated).
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// ResourceKind is the kind of the resource that triggered the event.
	// +kubebuilder:validation:Required
	ResourceKind string `json:"resourceKind"`

	// ResourceName is the name of the resource within its namespace.
	// +kubebuilder:validation:Required
	ResourceName string `json:"resourceName"`

	// Actor is the identity that initiated the action. For operator-driven events
	// this is "smeltry-operator"; for user-initiated deletions it carries the
	// requesting user's email when available via the OIDC token.
	// +optional
	Actor string `json:"actor,omitempty"`

	// OldPhase is the phase before the transition (only set for PhaseChanged events).
	// +optional
	OldPhase string `json:"oldPhase,omitempty"`

	// NewPhase is the phase after the transition (only set for PhaseChanged events).
	// +optional
	NewPhase string `json:"newPhase,omitempty"`

	// MachineID is the Netbox device ID (only set for MachineAllocated events).
	// +optional
	MachineID int `json:"machineID,omitempty"`

	// Timestamp is when the action occurred.
	// +kubebuilder:validation:Required
	Timestamp metav1.Time `json:"timestamp"`

	// TTL is the duration after which this AuditEvent will be automatically
	// deleted by the purge controller. Accepts Go duration strings (e.g. "720h").
	// If empty, the operator's default TTL applies.
	// +optional
	TTL string `json:"ttl,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ae

// AuditEvent records one auditable action performed by smeltry-operator.
// Events are immutable once created; deletion happens automatically via the
// TTL purge controller.
type AuditEvent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AuditEventSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// AuditEventList contains a list of AuditEvent.
type AuditEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AuditEvent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AuditEvent{}, &AuditEventList{})
}
