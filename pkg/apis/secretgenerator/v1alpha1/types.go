package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	ConditionTypeReady = "Ready"

	ReasonReconciled                = "Reconciled"
	ReasonInvalidSpec               = "InvalidSpec"
	ReasonLegacyBaselineInvalid     = "LegacyBaselineInvalid"
	ReasonSecretOwnershipConflict   = "SecretOwnershipConflict"
	ReasonRegenerationStateConflict = "RegenerationStateConflict"
	ReasonTrackingStateConflict     = "TrackingStateConflict"
	ReasonSecretSizeConflict        = "SecretSizeConflict"
	ReasonSecretTypeConflict        = "SecretTypeConflict"
	ReasonImmutableSecretConflict   = "ImmutableSecretConflict"
	ReasonGenerationFailed          = "GenerationFailed"
	ReasonApplyFailed               = "ApplyFailed"
)

// +k8s:deepcopy-gen=false
type SecretStatus interface {
	GetCommonStatus() *CommonSecretStatus
	GetSecret() *v1.ObjectReference
	SetSecret(secret *v1.ObjectReference)
}

// CommonSecretStatus contains the status contract shared by all generated
// Secret custom resources.
type CommonSecretStatus struct {
	// Secret is the stable identity of the managed Secret. ResourceVersion is
	// intentionally not recorded.
	// +optional
	Secret *v1.ObjectReference `json:"secret,omitempty"`
	// ObservedGeneration is the most recent generation processed by the controller.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions contains the current reconciliation state. Ready is the only
	// supported condition type.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// LastRegeneratedGeneration mirrors the authoritative marker stored on the
	// managed Secret. Controllers must not use this field for idempotency.
	// +optional
	// +kubebuilder:validation:Minimum=0
	LastRegeneratedGeneration int64 `json:"lastRegeneratedGeneration,omitempty"`
	// TrackingInitialized mirrors whether the managed Secret has a complete
	// controller tracking bundle.
	// +optional
	TrackingInitialized bool `json:"trackingInitialized,omitempty"`
}

func (in *CommonSecretStatus) GetSecret() *v1.ObjectReference {
	return in.Secret
}

func (in *CommonSecretStatus) GetCommonStatus() *CommonSecretStatus {
	return in
}

func (in *CommonSecretStatus) SetSecret(secret *v1.ObjectReference) {
	in.Secret = secret
}

type ReconcilerState string

// +k8s:deepcopy-gen=false
type APIObject interface {
	GetStatus() SecretStatus
	GetType() string
	runtime.Object
	metav1.Object
}
