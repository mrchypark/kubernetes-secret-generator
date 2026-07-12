package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SSHKeyPairSpec defines the desired state of SSHKeyPair
// +kubebuilder:validation:XValidation:rule="self.algorithm == 'ed25519' || !has(self.length) || (self.algorithm == 'rsa' && self.length in ['2048', '3072', '4096']) || (self.algorithm == 'ecdsa' && self.length in ['256', '384', '521'])",message="length must match the selected algorithm"
// +kubebuilder:validation:XValidation:rule="(has(self.privateKeyField) ? self.privateKeyField : 'ssh-privatekey') != (has(self.publicKeyField) ? self.publicKeyField : 'ssh-publickey')",message="privateKeyField and publicKeyField must differ"
// +kubebuilder:validation:XValidation:rule="!has(self.data) || !((has(self.privateKeyField) ? self.privateKeyField : 'ssh-privatekey') in self.data) && !((has(self.publicKeyField) ? self.publicKeyField : 'ssh-publickey') in self.data)",message="key fields must not collide with data keys"
// +kubebuilder:validation:XValidation:rule="(has(oldSelf.type) && oldSelf.type.size() > 0 ? oldSelf.type : 'Opaque') == (has(self.type) && self.type.size() > 0 ? self.type : 'Opaque')",message="type is immutable after creation; omitted, empty, and Opaque are equivalent"
type SSHKeyPairSpec struct {
	// +optional
	// +kubebuilder:default=rsa
	// +kubebuilder:validation:Enum=rsa;ecdsa;ed25519
	Algorithm string `json:"algorithm,omitempty"`
	// +optional
	Length string `json:"length,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	PrivateKey string `json:"privateKey,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	PrivateKeyField string `json:"privateKeyField,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	PublicKeyField string `json:"publicKeyField,omitempty"`
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxProperties=254
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() <= 253 && k.matches('^[A-Za-z0-9._-]+$'))",message="data keys must be 1..253 characters and contain only letters, digits, dot, underscore, or hyphen"
	Data map[string]string `json:"data,omitempty"`
	// +optional
	ForceRegenerate bool `json:"forceRegenerate,omitempty"`
}

// SSHKeyPairStatus defines the observed state of SSHKeyPair
type SSHKeyPairStatus struct {
	CommonSecretStatus `json:",inline"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SSHKeyPair is the Schema for the sshkeypairs API
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=sshkeypairs,scope=Namespaced
// +kubebuilder:metadata:annotations="secretgenerator.mittwald.de/schema-release=v4.0.0"
type SSHKeyPair struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SSHKeyPairSpec   `json:"spec,omitempty"`
	Status SSHKeyPairStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SSHKeyPairList contains a list of SSHKeyPair
type SSHKeyPairList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SSHKeyPair `json:"items"`
}

func (in *SSHKeyPairList) GetTypeMeta() metav1.TypeMeta {
	return in.TypeMeta
}

func (in *SSHKeyPairList) SetTypeMeta(meta metav1.TypeMeta) {
	in.TypeMeta = meta
}

func (in *SSHKeyPairList) GetListMeta() metav1.ListMeta {
	return in.ListMeta
}

func (in *SSHKeyPairList) SetListMeta(meta metav1.ListMeta) {
	in.ListMeta = meta
}

func (in *SSHKeyPair) GetPrivateKeyField() string {
	if in.Spec.PrivateKeyField != "" {
		return in.Spec.PrivateKeyField
	}
	return "ssh-privatekey"
}

func (in *SSHKeyPair) GetPublicKeyField() string {
	if in.Spec.PublicKeyField != "" {
		return in.Spec.PublicKeyField
	}
	return "ssh-publickey"
}

func (in *SSHKeyPair) GetStatus() SecretStatus {
	return &in.Status
}

func (in *SSHKeyPair) GetType() string {
	return in.Spec.Type
}
