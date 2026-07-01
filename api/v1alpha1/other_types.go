package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ── ServerClaim ────────────────────────────────────────────────────────────

type ServerClaimPhase string

const (
	ServerClaimPhasePending      ServerClaimPhase = "Pending"
	ServerClaimPhaseProvisioning ServerClaimPhase = "Provisioning"
	ServerClaimPhaseReady        ServerClaimPhase = "Ready"
	ServerClaimPhaseFailed       ServerClaimPhase = "Failed"
)

// ServerClaimSpec defines the desired state of ServerClaim.
type ServerClaimSpec struct {
	// MachineClass is the Netbox device model used to filter available hardware.
	// +kubebuilder:validation:Required
	MachineClass string `json:"machineClass"`

	// Site is the Netbox site slug.
	// +kubebuilder:validation:Required
	Site string `json:"site"`

	// OS is the operating system variant passed to the Tinkerbell workflow template.
	// Examples: flatcar, ubuntu-2404, rocky9
	// +kubebuilder:validation:Required
	OS string `json:"os"`
}

// ServerClaimStatus defines the observed state of ServerClaim.
type ServerClaimStatus struct {
	// Phase is the current lifecycle phase.
	// +kubebuilder:default=Pending
	Phase ServerClaimPhase `json:"phase,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ServerIP is the IP allocated from Netbox IPAM.
	ServerIP string `json:"serverIP,omitempty"`

	// ServerDNS is the DNS name registered in CoreDNS.
	ServerDNS string `json:"serverDNS,omitempty"`

	// NetboxIPAMID is the Netbox IP address record ID, for deletion on finalizer.
	NetboxIPAMID int `json:"netboxIPAMID,omitempty"`

	// AllocatedMachineID is the Netbox device ID, for release on finalizer.
	AllocatedMachineID int `json:"allocatedMachineID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.serverIP`
// +kubebuilder:printcolumn:name="OS",type=string,JSONPath=`.spec.os`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ServerClaim is the Schema for the serverclaims API.
// A tenant creates one ServerClaim to request a plain OS server on bare metal.
type ServerClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerClaimSpec   `json:"spec,omitempty"`
	Status ServerClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServerClaimList contains a list of ServerClaim.
type ServerClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServerClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServerClaim{}, &ServerClaimList{})
}

// ── AddonProfile ───────────────────────────────────────────────────────────

// HelmRef identifies a Helm chart in a repository.
type HelmRef struct {
	// RepoURL is the Helm repository URL.
	RepoURL string `json:"repoURL"`

	// ChartName is the name of the chart within the repository.
	ChartName string `json:"chartName"`

	// ChartVersion is the exact chart version to deploy (e.g. "1.16.2").
	ChartVersion string `json:"chartVersion"`

	// Values is an optional inline YAML string of Helm values.
	// For rook-ceph, OSD device mappings are merged in at reconciliation time.
	// +optional
	Values string `json:"values,omitempty"`
}

// AddonComponent describes one addon in a profile.
type AddonComponent struct {
	// Name is a human-readable identifier for this component (e.g. "cilium", "rook-ceph").
	Name string `json:"name"`

	// Required components must be healthy before ClusterClaim reaches Ready.
	// +kubebuilder:default=true
	Required bool `json:"required"`

	// Order controls installation sequence. Lower values install first.
	// +kubebuilder:validation:Minimum=1
	Order int `json:"order"`

	// HelmRef points to the Helm chart to deploy for this component.
	HelmRef HelmRef `json:"helmRef"`
}

// MachineConstraints restricts which machine classes are compatible with a profile.
type MachineConstraints struct {
	// RequiredTags are Netbox device tags that must ALL be present on the machine.
	RequiredTags []string `json:"requiredTags,omitempty"`
}

// AddonProfileSpec defines the desired state of AddonProfile.
type AddonProfileSpec struct {
	// Description is shown to tenants in the Web UI and CLI.
	Description string `json:"description,omitempty"`

	// Components is the ordered list of addons to deploy on matching clusters.
	// +kubebuilder:validation:MinItems=1
	Components []AddonComponent `json:"components"`

	// MachineConstraints optionally restricts which machineClasses are compatible.
	MachineConstraints *MachineConstraints `json:"machineConstraints,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ap
// +kubebuilder:printcolumn:name="Description",type=string,JSONPath=`.spec.description`

// AddonProfile is the Schema for the addonprofiles API.
// Defined by admins in portal-system; readable by tenants.
type AddonProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AddonProfileSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// AddonProfileList contains a list of AddonProfile.
type AddonProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AddonProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AddonProfile{}, &AddonProfileList{})
}

// ── SiteConfig ─────────────────────────────────────────────────────────────

// SiteNetboxConfig holds Netbox-specific parameters for a site.
type SiteNetboxConfig struct {
	// SiteSlug is the Netbox site slug (e.g. "paris-dc1").
	SiteSlug string `json:"siteSlug"`

	// ProvisioningPrefix is the IPAM prefix from which IPs are allocated.
	// Example: "10.0.1.0/24"
	ProvisioningPrefix string `json:"provisioningPrefix"`

	// IPAMTags are Netbox tags applied to every IP reserved by the operator.
	IPAMTags []string `json:"ipamTags,omitempty"`
}

// SiteNetworkConfig holds network topology parameters for a site.
type SiteNetworkConfig struct {
	// ProvisioningCIDR is the network reachable by workers at PXE boot time.
	ProvisioningCIDR string `json:"provisioningCIDR"`

	// ManagementCIDR is the out-of-band management network (BMC access).
	ManagementCIDR string `json:"managementCIDR"`
}

// SiteCiliumConfig holds Cilium-specific parameters for a site.
type SiteCiliumConfig struct {
	// L2PoolName is the name of the CiliumLoadBalancerIPPool on the management cluster.
	// Used for both the control plane Service and the tenant ingress Service.
	L2PoolName string `json:"l2PoolName"`
}

// SiteDNSConfig holds DNS parameters for a site.
type SiteDNSConfig struct {
	// Zone is the DNS zone managed by CoreDNS + Netbox IPAM.
	// Control plane: <cluster>-api.<tenant>.<zone>
	// Webhook:       <cluster>-wh.<tenant>.<zone>
	Zone string `json:"zone"`
}

// SiteOIDCConfig holds OIDC parameters injected into each TenantControlPlane kube-apiserver.
type SiteOIDCConfig struct {
	// IssuerURL is the OIDC issuer URL (--oidc-issuer-url).
	IssuerURL string `json:"issuerURL"`

	// ClientID is the OIDC client ID (--oidc-client-id).
	ClientID string `json:"clientID"`

	// UsernameClaim is the JWT claim used as the Kubernetes username (--oidc-username-claim).
	// +kubebuilder:default=email
	UsernameClaim string `json:"usernameClaim"`

	// GroupsClaim is the JWT claim used as Kubernetes groups (--oidc-groups-claim).
	// +kubebuilder:default=groups
	GroupsClaim string `json:"groupsClaim"`
}

// SiteConfigSpec defines the desired state of SiteConfig.
type SiteConfigSpec struct {
	Netbox  SiteNetboxConfig  `json:"netbox"`
	Network SiteNetworkConfig `json:"network"`
	Cilium  SiteCiliumConfig  `json:"cilium"`
	DNS     SiteDNSConfig     `json:"dns"`
	OIDC    SiteOIDCConfig    `json:"oidc"`
}

// MachineCharacteristics summarizes hardware specifications for a machine class.
// Values are derived from the Netbox device type; fields are omitted if not available.
type MachineCharacteristics struct {
	CPUCores  int `json:"cpuCores,omitempty"`
	RAMGB     int `json:"ramGB,omitempty"`
	StorageGB int `json:"storageGB,omitempty"`
}

// MachineClassSummary aggregates machine availability for one class on a site.
// The operator populates this from Netbox and stores it in SiteConfig.status.machineClasses.
type MachineClassSummary struct {
	// MachineClass is the Netbox device model name (matches ClusterClaim.spec.machineClass).
	MachineClass string `json:"machineClass"`

	// AvailableCount is the number of active, unassigned devices of this class on the site.
	AvailableCount int `json:"availableCount"`

	// Tags are the Netbox device tags present on the devices of this class.
	Tags []string `json:"tags,omitempty"`

	// Characteristics summarizes hardware specs; may be empty if not available in Netbox.
	Characteristics MachineCharacteristics `json:"characteristics,omitempty"`
}

// SiteConfigStatus reflects validation results and machine availability synced from Netbox.
type SiteConfigStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// MachineClasses lists available machine classes on this site, refreshed from Netbox.
	// An empty slice (not nil) means the site was synced but has no active machines.
	// Nil means the site has never been synced.
	MachineClasses []MachineClassSummary `json:"machineClasses"`

	// LastMachineSync is the timestamp of the last successful Netbox machine sync.
	LastMachineSync *metav1.Time `json:"lastMachineSync,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=site
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.spec.netbox.provisioningPrefix`
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.dns.zone`

// SiteConfig is the Schema for the siteconfigs API.
// Defined by admins in portal-system. Validated by an admission webhook.
type SiteConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SiteConfigSpec   `json:"spec,omitempty"`
	Status SiteConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SiteConfigList contains a list of SiteConfig.
type SiteConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SiteConfig{}, &SiteConfigList{})
}
