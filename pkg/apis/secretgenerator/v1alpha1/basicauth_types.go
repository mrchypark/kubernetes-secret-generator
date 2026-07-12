package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// BasicAuthSpec defines the desired state of BasicAuth
// +kubebuilder:validation:XValidation:rule="!has(self.data) || !(['auth', 'username', 'password'].exists(k, k in self.data))",message="data must not contain reserved keys auth, username, or password"
// +kubebuilder:validation:XValidation:rule="!has(self.rotationInterval) || self.rotationInterval.size() == 0 || (duration(self.rotationInterval) >= duration('1m') && duration(self.rotationInterval) <= duration('8760h'))",message="rotationInterval must be a Go duration between 1m and 8760h"
type BasicAuthSpec struct {
	// +optional
	// +kubebuilder:validation:Pattern=`^(?:[1-9]|[1-9][0-9]{1,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-6])[bB]?$`
	Length string `json:"length,omitempty"`
	// +optional
	// +kubebuilder:default=admin
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^:\r\n\x00]+$`
	Username string `json:"username,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=base64;base64url;base32;hex;raw
	Encoding string `json:"encoding,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxProperties=253
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() <= 253 && k.matches('^[A-Za-z0-9._-]+$'))",message="data keys must be 1..253 characters and contain only letters, digits, dot, underscore, or hyphen"
	Data map[string]string `json:"data,omitempty"`
	// +optional
	ForceRegenerate bool `json:"forceRegenerate,omitempty"`
	// RotationInterval periodically rotates the generated credential set. It
	// uses Go duration syntax; an empty value disables periodic rotation.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	RotationInterval string `json:"rotationInterval,omitempty"`
}

// BasicAuthStatus defines the observed state of BasicAuth
type BasicAuthStatus struct {
	CommonSecretStatus `json:",inline"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// BasicAuth is the Schema for the basicauths API
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=basicauths,scope=Namespaced
// +kubebuilder:metadata:annotations="secretgenerator.mittwald.de/schema-release=v4.0.0"
type BasicAuth struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BasicAuthSpec   `json:"spec,omitempty"`
	Status BasicAuthStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// BasicAuthList contains a list of BasicAuth
type BasicAuthList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BasicAuth `json:"items"`
}

func (in *BasicAuthList) GetTypeMeta() metav1.TypeMeta {
	return in.TypeMeta
}

func (in *BasicAuthList) SetTypeMeta(meta metav1.TypeMeta) {
	in.TypeMeta = meta
}

func (in *BasicAuthList) GetListMeta() metav1.ListMeta {
	return in.ListMeta
}

func (in *BasicAuthList) SetListMeta(meta metav1.ListMeta) {
	in.ListMeta = meta
}

func (in *BasicAuth) GetStatus() SecretStatus {
	return &in.Status
}

func (in *BasicAuth) GetType() string {
	return "Opaque"
}
