package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterClaimPhase represents the lifecycle phase of a ClusterClaim.
type ClusterClaimPhase string

const (
	ClusterClaimPhasePending      ClusterClaimPhase = "Pending"
	ClusterClaimPhaseProvisioning ClusterClaimPhase = "Provisioning"
	ClusterClaimPhaseClusterReady ClusterClaimPhase = "ClusterReady"
	ClusterClaimPhaseAddonsReady  ClusterClaimPhase = "AddonsReady"
	ClusterClaimPhaseReady        ClusterClaimPhase = "Ready"
	ClusterClaimPhaseFailed       ClusterClaimPhase = "Failed"
)

// Condition types for ClusterClaim.
const (
	ConditionValidated      = "Validated"
	ConditionIPAllocated    = "IPAllocated"
	ConditionHWAllocated    = "HardwareAllocated"
	ConditionMachineCfgDone = "MachineCfgDone"
	ConditionCAPIReady      = "CAPIReady"
	ConditionAddonsReady    = "AddonsReady"
)

// ClusterClaimSpec defines the desired state of ClusterClaim.
// The user fills this; the operator resolves everything else.
type ClusterClaimSpec struct {
	// MachineClass is the Netbox device model used to filter available hardware.
	// +kubebuilder:validation:Required
	MachineClass string `json:"machineClass"`

	// MachineCount is the number of worker nodes to allocate.
	// +kubebuilder:validation:Minimum=1
	MachineCount int `json:"machineCount"`

	// Site is the Netbox site slug. Resolved to network parameters via SiteConfig.
	// +kubebuilder:validation:Required
	Site string `json:"site"`

	// AddonProfile is the name of an AddonProfile in portal-system namespace.
	// +kubebuilder:validation:Required
	AddonProfile string `json:"addonProfile"`
}

// ClusterClaimStatus defines the observed state of ClusterClaim.
type ClusterClaimStatus struct {
	// Phase is the current lifecycle phase.
	// +kubebuilder:default=Pending
	Phase ClusterClaimPhase `json:"phase,omitempty"`

	// Conditions holds detailed status for each reconciliation step.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ControlPlaneIP is the IP allocated in Netbox IPAM for the kube-apiserver.
	ControlPlaneIP string `json:"controlPlaneIP,omitempty"`

	// ControlPlaneDNS is the DNS name registered in CoreDNS for the control plane.
	ControlPlaneDNS string `json:"controlPlaneDNS,omitempty"`

	// WebhookIP is the IP allocated in Netbox IPAM for the tenant ingress (webhooks).
	WebhookIP string `json:"webhookIP,omitempty"`

	// WebhookDNS is the DNS name registered in CoreDNS for the webhook ingress.
	WebhookDNS string `json:"webhookDNS,omitempty"`

	// NetboxIPAMIDs holds the Netbox IP address IDs to DELETE on finalizer.
	NetboxIPAMIDs []int `json:"netboxIPAMIDs,omitempty"`

	// AllocatedMachineIDs holds the Netbox device IDs allocated to this cluster.
	AllocatedMachineIDs []int `json:"allocatedMachineIDs,omitempty"`

	// OSDDevices maps Netbox device ID (as string) to a list of block device names
	// tagged as ceph-osd in Netbox inventory items.
	// Example: {"42": ["sdb", "sdc"], "57": ["sdb"]}
	OSDDevices map[string][]string `json:"osdDevices,omitempty"`

	// KubeconfigSecret is the name of the Secret in the tenant namespace
	// containing the cluster kubeconfig. Populated when phase=Ready.
	KubeconfigSecret string `json:"kubeconfigSecret,omitempty"`

	// ClusterRef is a reference to the underlying CAPI Cluster object.
	ClusterRef *LocalObjectRef `json:"clusterRef,omitempty"`
}

// LocalObjectRef is a reference to an object in the same namespace.
type LocalObjectRef struct {
	Name string `json:"name"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="ControlPlaneIP",type=string,JSONPath=`.status.controlPlaneIP`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.spec.machineCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterClaim is the Schema for the clusterclaims API.
// A tenant creates one ClusterClaim to request a Kubernetes cluster on bare metal.
type ClusterClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterClaimSpec   `json:"spec,omitempty"`
	Status ClusterClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterClaimList contains a list of ClusterClaim.
type ClusterClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterClaim{}, &ClusterClaimList{})
}
