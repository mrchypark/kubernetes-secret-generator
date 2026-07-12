package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// StringSecretSpec defines the desired state of StringSecret
// +kubebuilder:validation:XValidation:rule="(has(self.data) && size(self.data) > 0) || (has(self.fields) && size(self.fields) > 0)",message="at least one of data or fields must be non-empty"
// +kubebuilder:validation:XValidation:rule="!has(self.fields) || self.fields.all(f, self.fields.filter(other, other.fieldName == f.fieldName).size() == 1)",message="fieldName values must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.data) || !has(self.fields) || self.fields.all(f, !(f.fieldName in self.data))",message="generated fieldName values must not collide with data keys"
// +kubebuilder:validation:XValidation:rule="(has(self.data) ? size(self.data) : 0) + (has(self.fields) ? size(self.fields) : 0) <= 256",message="data and fields may manage at most 256 keys"
// +kubebuilder:validation:XValidation:rule="(has(oldSelf.type) && oldSelf.type.size() > 0 ? oldSelf.type : 'Opaque') == (has(self.type) && self.type.size() > 0 ? self.type : 'Opaque')",message="type is immutable after creation; omitted, empty, and Opaque are equivalent"
type StringSecretSpec struct {
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() <= 253 && k.matches('^[A-Za-z0-9._-]+$'))",message="data keys must be 1..253 characters and contain only letters, digits, dot, underscore, or hyphen"
	Data map[string]string `json:"data,omitempty"`
	// +optional
	ForceRegenerate bool `json:"forceRegenerate,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Fields []Field `json:"fields,omitempty"`
}

type Field struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	FieldName string `json:"fieldName"`
	// +optional
	// +kubebuilder:validation:Enum=base64;base64url;base32;hex;raw
	Encoding string `json:"encoding,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^(?:[1-9]|[1-9][0-9]{1,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-6])[bB]?$`
	Length string `json:"length,omitempty"`
}

// StringSecretStatus defines the observed state of StringSecret
type StringSecretStatus struct {
	CommonSecretStatus `json:",inline"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// StringSecret is the Schema for the stringsecrets API
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=stringsecrets,scope=Namespaced
// +kubebuilder:metadata:annotations="secretgenerator.mittwald.de/schema-release=v4.0.0"
type StringSecret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              StringSecretSpec   `json:"spec,omitempty"`
	Status            StringSecretStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// StringSecretList contains a list of StringSecret
type StringSecretList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StringSecret `json:"items"`
}

func (in *StringSecretList) GetTypeMeta() metav1.TypeMeta {
	return in.TypeMeta
}

func (in *StringSecretList) SetTypeMeta(meta metav1.TypeMeta) {
	in.TypeMeta = meta
}

func (in *StringSecretList) GetListMeta() metav1.ListMeta {
	return in.ListMeta
}

func (in *StringSecretList) SetListMeta(meta metav1.ListMeta) {
	in.ListMeta = meta
}

func (in *StringSecret) GetStatus() SecretStatus {
	return &in.Status
}

func (in *StringSecret) GetType() string {
	return in.Spec.Type
}
