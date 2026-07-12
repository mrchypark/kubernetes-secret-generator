package crd

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	controllerobservability "github.com/mittwald/kubernetes-secret-generator/pkg/controller/observability"
)

const (
	AnnotationTrackingVersion       = "secretgenerator.mittwald.de/tracking-version"
	AnnotationManagedDataKeys       = "secretgenerator.mittwald.de/managed-data-keys"
	AnnotationManagedLabelKeys      = "secretgenerator.mittwald.de/managed-label-keys"
	AnnotationManagedDataChecksums  = "secretgenerator.mittwald.de/managed-data-checksums"
	AnnotationGenerationFingerprint = "secretgenerator.mittwald.de/generation-spec-fingerprints"
	AnnotationManagedKeysDigest     = "secretgenerator.mittwald.de/managed-keys-digest"
	AnnotationLastRegenerated       = "secretgenerator.mittwald.de/last-regenerated-generation"
	TrackingVersion                 = "v1"
)

var trackingAnnotations = []string{
	AnnotationTrackingVersion, AnnotationManagedDataKeys, AnnotationManagedLabelKeys,
	AnnotationManagedDataChecksums, AnnotationGenerationFingerprint, AnnotationManagedKeysDigest,
	AnnotationLastRegenerated,
}

type TrackingState int

const (
	TrackingAbsent TrackingState = iota
	TrackingValid
	TrackingChecksumIncomplete
	TrackingConflict
)

type TrackingAction int

const (
	TrackingBaseline TrackingAction = iota
	TrackingReconstructStatus
	TrackingReconcile
	TrackingRepairChecksums
	TrackingReject
)

type Tracking struct {
	DataKeys     []string
	LabelKeys    []string
	Checksums    map[string]string
	Fingerprints map[string]string
}

// ClassifyTracking is the pure status/Secret tracking state machine. The
// status bit prevents a previously initialized Secret with lost tracking from
// being adopted as a new legacy baseline.
func ClassifyTracking(initialized bool, state TrackingState) TrackingAction {
	if !initialized {
		switch state {
		case TrackingAbsent:
			return TrackingBaseline
		case TrackingValid:
			return TrackingReconstructStatus
		default:
			return TrackingReject
		}
	}
	switch state {
	case TrackingValid:
		return TrackingReconcile
	case TrackingChecksumIncomplete:
		return TrackingRepairChecksums
	default:
		return TrackingReject
	}
}

// NewSecret is retained for callers and tests that construct a new owned Secret.
func NewSecret(owner metav1.Object, values map[string][]byte, secretType string) (*corev1.Secret, error) {
	return NewSecretWithScheme(owner, values, secretType, scheme.Scheme)
}

func NewSecretWithScheme(owner metav1.Object, values map[string][]byte, secretType string, objectScheme *runtime.Scheme) (*corev1.Secret, error) {
	t := corev1.SecretType(secretType)
	if t == "" {
		t = corev1.SecretTypeOpaque
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: owner.GetName(), Namespace: owner.GetNamespace(), Labels: cloneStrings(owner.GetLabels()),
	}, Type: t, Data: values}
	if err := controllerutil.SetControllerReference(owner, secret, objectScheme); err != nil {
		return nil, err
	}
	return secret, nil
}

func CheckError(err error) (reconcile.Result, error) {
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, err
}

func IgnoreStatusUpdatePredicate[T client.Object]() predicate.TypedPredicate[T] {
	return ResourceChangePredicateFor[T]("")
}

func ResourceChangePredicateFor[T client.Object](controller string) predicate.TypedPredicate[T] {
	return predicate.TypedFuncs[T]{
		CreateFunc: func(e event.TypedCreateEvent[T]) bool {
			if controller != "" {
				controllerobservability.ObserveEligibleEvent(controller, "resource_create", e.Object)
			}
			return true
		},
		UpdateFunc: func(e event.TypedUpdateEvent[T]) bool {
			changed := e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() || !reflect.DeepEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels())
			if changed && controller != "" {
				controllerobservability.ObserveEligibleEvent(controller, "resource_update", e.ObjectNew)
			}
			return changed
		},
		DeleteFunc: func(e event.TypedDeleteEvent[T]) bool {
			accepted := !e.DeleteStateUnknown
			if accepted && controller != "" {
				controllerobservability.ObserveEligibleEvent(controller, "resource_delete", e.Object)
			}
			return accepted
		},
	}
}

// SecretChangePredicate ignores updates that cannot affect desired state. The owner handler
// itself reads both old and new objects, so owner removal and replacement enqueue the old CR.
func SecretChangePredicate() predicate.TypedPredicate[*corev1.Secret] {
	return SecretChangePredicateFor("", schema.GroupVersionKind{})
}

func SecretChangePredicateFor(controller string, ownerGVK schema.GroupVersionKind) predicate.TypedPredicate[*corev1.Secret] {
	return predicate.TypedFuncs[*corev1.Secret]{
		CreateFunc: func(event.TypedCreateEvent[*corev1.Secret]) bool { return true },
		DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Secret]) bool {
			observeSecretOwners(controller, "secret_delete", ownerGVK, false, false, e.Object, e.Object)
			return true
		},
		UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Secret]) bool {
			old, current := e.ObjectOld, e.ObjectNew
			dataChanged := !reflect.DeepEqual(old.Data, current.Data)
			trackingChanged := !reflect.DeepEqual(controllerAnnotations(old.Annotations), controllerAnnotations(current.Annotations))
			changed := dataChanged || old.Type != current.Type ||
				!equalBoolPtr(old.Immutable, current.Immutable) ||
				!reflect.DeepEqual(old.Labels, current.Labels) ||
				!reflect.DeepEqual(old.OwnerReferences, current.OwnerReferences) ||
				trackingChanged
			if changed {
				expected := controllerobservability.ConsumeControllerWrite(controller, current)
				validTrackingAdvance := false
				if trackingChanged && !expected {
					// A cold cache has no prior object with which to prove provenance.
					// For observed updates, a newly valid tracking bundle is fail-closed.
					_, state, _ := LoadTracking(current)
					validTrackingAdvance = state == TrackingValid
				}
				observeSecretOwners(controller, "secret_drift", ownerGVK, (dataChanged || validTrackingAdvance) && !expected, trackingChanged && !expected, current, old, current)
			}
			return changed
		},
	}
}

func observeSecretOwners(controller, source string, ownerGVK schema.GroupVersionKind, dataChanged, trackingChanged bool, subject *corev1.Secret, ownerSources ...*corev1.Secret) {
	if controller == "" {
		return
	}
	type ownerKey struct {
		types.NamespacedName
		uid types.UID
	}
	seen := map[ownerKey]bool{}
	for _, ownerSource := range ownerSources {
		for _, ref := range ownerSource.OwnerReferences {
			if ref.Controller == nil || !*ref.Controller || ref.APIVersion != ownerGVK.GroupVersion().String() || ref.Kind != ownerGVK.Kind {
				continue
			}
			key := ownerKey{NamespacedName: types.NamespacedName{Namespace: subject.Namespace, Name: ref.Name}, uid: ref.UID}
			if !seen[key] {
				owner := &metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, UID: ref.UID}
				controllerobservability.ObserveOwnedSecretCandidate(controller, source, owner, subject, dataChanged, trackingChanged)
				seen[key] = true
			}
		}
	}
}

func ExactControllerOwner(secret *corev1.Secret, owner metav1.Object, gvk schema.GroupVersionKind) bool {
	if len(secret.OwnerReferences) != 1 {
		return false
	}
	ref := secret.OwnerReferences[0]
	return ref.Controller != nil && *ref.Controller && ref.APIVersion == gvk.GroupVersion().String() &&
		ref.Kind == gvk.Kind && ref.Name == owner.GetName() && ref.UID == owner.GetUID()
}

func Fingerprint(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		var n [8]byte
		v := uint64(len(part))
		for i := 7; i >= 0; i-- {
			n[i] = byte(v)
			v >>= 8
		}
		h.Write(n[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func DataChecksum(key string, value []byte) string { return Fingerprint([]byte(key), value) }

func EffectiveSecretType(value string) corev1.SecretType {
	if value == "" {
		return corev1.SecretTypeOpaque
	}
	return corev1.SecretType(value)
}

func SecretTypeMatches(secret *corev1.Secret, desired corev1.SecretType) bool {
	return EffectiveSecretType(string(secret.Type)) == desired
}

func LoadTracking(secret *corev1.Secret) (*Tracking, TrackingState, error) {
	a := secret.Annotations
	present := 0
	for _, key := range trackingAnnotations[:6] {
		if _, ok := a[key]; ok {
			present++
		}
	}
	if present == 0 {
		return nil, TrackingAbsent, nil
	}
	if present != 6 || a[AnnotationTrackingVersion] != TrackingVersion {
		return nil, TrackingConflict, fmt.Errorf("tracking bundle is partial or has an unsupported version")
	}
	t := &Tracking{}
	if err := json.Unmarshal([]byte(a[AnnotationManagedDataKeys]), &t.DataKeys); err != nil || !canonicalList(t.DataKeys) {
		return nil, TrackingConflict, fmt.Errorf("managed data key list is malformed")
	}
	if err := json.Unmarshal([]byte(a[AnnotationManagedLabelKeys]), &t.LabelKeys); err != nil || !canonicalList(t.LabelKeys) {
		return nil, TrackingConflict, fmt.Errorf("managed label key list is malformed")
	}
	if Fingerprint(mustJSON(t.DataKeys), mustJSON(t.LabelKeys)) != a[AnnotationManagedKeysDigest] {
		return nil, TrackingConflict, fmt.Errorf("managed key list digest does not match")
	}
	if err := json.Unmarshal([]byte(a[AnnotationGenerationFingerprint]), &t.Fingerprints); err != nil || t.Fingerprints == nil {
		return nil, TrackingConflict, fmt.Errorf("generation fingerprint map is malformed")
	}
	for key, fingerprint := range t.Fingerprints {
		if key == "" || !validSHA256(fingerprint) {
			return nil, TrackingConflict, fmt.Errorf("generation fingerprint entry is malformed")
		}
	}
	if err := json.Unmarshal([]byte(a[AnnotationManagedDataChecksums]), &t.Checksums); err != nil || t.Checksums == nil {
		return t, TrackingChecksumIncomplete, fmt.Errorf("managed data checksum map is malformed")
	}
	for key, sum := range t.Checksums {
		if key == "" || !validSHA256(sum) {
			return t, TrackingChecksumIncomplete, fmt.Errorf("managed data checksum entry is malformed")
		}
	}
	for _, key := range t.DataKeys {
		if _, ok := t.Checksums[key]; !ok {
			return t, TrackingChecksumIncomplete, fmt.Errorf("managed data checksum entry is missing")
		}
	}
	return t, TrackingValid, nil
}

func RequireFingerprints(tracking *Tracking, keys ...string) error {
	if tracking == nil {
		return nil
	}
	for _, key := range keys {
		if _, ok := tracking.Fingerprints[key]; !ok {
			return fmt.Errorf("generation fingerprint is missing")
		}
	}
	return nil
}

func StoreTracking(secret *corev1.Secret, tracking *Tracking) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	tracking.DataKeys = sortedUnique(tracking.DataKeys)
	tracking.LabelKeys = sortedUnique(tracking.LabelKeys)
	secret.Annotations[AnnotationTrackingVersion] = TrackingVersion
	secret.Annotations[AnnotationManagedDataKeys] = string(mustJSON(tracking.DataKeys))
	secret.Annotations[AnnotationManagedLabelKeys] = string(mustJSON(tracking.LabelKeys))
	secret.Annotations[AnnotationManagedDataChecksums] = string(mustJSON(tracking.Checksums))
	secret.Annotations[AnnotationGenerationFingerprint] = string(mustJSON(tracking.Fingerprints))
	secret.Annotations[AnnotationManagedKeysDigest] = Fingerprint(mustJSON(tracking.DataKeys), mustJSON(tracking.LabelKeys))
}

func ParseRegenerationMarker(secret *corev1.Secret, generation int64) (int64, error) {
	value, ok := secret.Annotations[AnnotationLastRegenerated]
	if !ok {
		return -1, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || parsed > generation {
		return 0, fmt.Errorf("regeneration marker is invalid")
	}
	return parsed, nil
}

func SetRegenerationMarker(secret *corev1.Secret, generation int64) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[AnnotationLastRegenerated] = strconv.FormatInt(generation, 10)
}

func ApplyManagedData(target *corev1.Secret, literals map[string]string, desiredKeys, previousKeys []string) {
	if target.Data == nil {
		target.Data = map[string][]byte{}
	}
	desired := stringSet(desiredKeys)
	for _, key := range previousKeys {
		if _, ok := desired[key]; !ok {
			delete(target.Data, key)
		}
	}
	for key, value := range literals {
		target.Data[key] = []byte(value)
	}
}

func ApplyManagedLabels(target *corev1.Secret, labels map[string]string, previous []string) {
	if target.Labels == nil && (len(labels) > 0 || len(previous) > 0) {
		target.Labels = map[string]string{}
	}
	for _, key := range previous {
		if _, ok := labels[key]; !ok {
			delete(target.Labels, key)
		}
	}
	for key, value := range labels {
		target.Labels[key] = value
	}
}

func RefreshChecksums(t *Tracking, data map[string][]byte) {
	t.Checksums = make(map[string]string, len(t.DataKeys))
	for _, key := range t.DataKeys {
		t.Checksums[key] = DataChecksum(key, data[key])
	}
}

// ReserveChecksums makes projected-size validation account for the final
// checksum keys and fixed-width SHA-256 values before credential allocation.
func ReserveChecksums(t *Tracking) {
	t.Checksums = make(map[string]string, len(t.DataKeys))
	for _, key := range t.DataKeys {
		t.Checksums[key] = DataChecksum(key, nil)
	}
}

func SameStrings(a, b []string) bool {
	return reflect.DeepEqual(sortedUnique(a), sortedUnique(b))
}

func ValidateGeneratedValue(value []byte, length int, byteLength bool, encoding string) error {
	if len(value) == 0 {
		return fmt.Errorf("generated value is empty")
	}
	if !byteLength {
		if len(value) != length {
			return fmt.Errorf("generated value length does not match")
		}
		return validateEncodedPrefix(value, encoding)
	}
	decoded, err := decodeValue(value, encoding, false)
	if err != nil || len(decoded) != length {
		return fmt.Errorf("generated value encoding or byte length does not match")
	}
	return nil
}

func validateEncodedPrefix(value []byte, encoding string) error {
	for _, c := range value {
		valid := false
		switch encoding {
		case "raw":
			return nil
		case "base64":
			valid = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
		case "base64url":
			valid = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		case "base32":
			valid = c >= 'A' && c <= 'Z' || c >= '2' && c <= '7'
		case "hex":
			valid = c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
		}
		if !valid {
			return fmt.Errorf("generated value is not canonical %s", encoding)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type Client struct{ client.Client }

func (c *Client) PatchSecret(ctx context.Context, existing, desired *corev1.Secret) (bool, error) {
	if existing.Type != desired.Type {
		return false, fmt.Errorf("secret type is immutable")
	}
	if secretSemanticallyEqual(existing, desired) {
		return false, nil
	}
	controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
	if err := c.Patch(ctx, desired, client.MergeFrom(existing)); err != nil {
		return true, err
	}
	controllerobservability.RecordControllerWrite(ctx, desired)
	return true, nil
}

func (c *Client) SetStatus(ctx context.Context, instance v1alpha1.APIObject, secret *corev1.Secret, ready metav1.ConditionStatus, reason, message string, tracking bool, lastRegenerated int64) error {
	base := instance.DeepCopyObject().(v1alpha1.APIObject)
	status := instance.GetStatus().GetCommonStatus()
	oldCondition := apiMeta.FindStatusCondition(base.GetStatus().GetCommonStatus().Conditions, v1alpha1.ConditionTypeReady)
	status.ObservedGeneration = instance.GetGeneration()
	status.TrackingInitialized = tracking
	if lastRegenerated >= 0 {
		status.LastRegeneratedGeneration = lastRegenerated
	}
	if secret != nil {
		status.Secret = &corev1.ObjectReference{APIVersion: "v1", Kind: "Secret", Namespace: secret.Namespace, Name: secret.Name, UID: secret.UID}
	}
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionTypeReady, Status: ready, Reason: reason, Message: message,
		ObservedGeneration: instance.GetGeneration(),
	})
	controllerobservability.SetConditionOutcome(ctx, ready, reason)
	if reflect.DeepEqual(base.GetStatus().GetCommonStatus(), status) {
		return nil
	}
	if err := c.Status().Patch(ctx, instance, client.MergeFrom(base)); err != nil {
		return err
	}
	controllerobservability.RecordConditionTransition(ctx, instance, oldCondition, ready, reason)
	return nil
}

func TerminalStatus(ctx context.Context, c client.Client, instance v1alpha1.APIObject, secret *corev1.Secret, reason, message string, tracking bool, last int64) (reconcile.Result, error) {
	if secret != nil {
		controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
	}
	cc := Client{Client: c}
	if err := cc.SetStatus(ctx, instance, secret, metav1.ConditionFalse, reason, message, tracking, last); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func ReadyStatus(ctx context.Context, c client.Client, instance v1alpha1.APIObject, secret *corev1.Secret, last int64) (reconcile.Result, error) {
	cc := Client{Client: c}
	if err := cc.SetStatus(ctx, instance, secret, metav1.ConditionTrue, v1alpha1.ReasonReconciled, "Secret is reconciled", true, last); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func ApplyFailure(ctx context.Context, c client.Client, instance v1alpha1.APIObject, secret *corev1.Secret, applyErr error) (reconcile.Result, error) {
	cc := Client{Client: c}
	_ = cc.SetStatus(ctx, instance, secret, metav1.ConditionFalse, v1alpha1.ReasonApplyFailed, "Secret apply failed", instance.GetStatus().GetCommonStatus().TrackingInitialized, -1)
	return reconcile.Result{}, applyErr
}

func ControllerGVK(kind string) schema.GroupVersionKind {
	return v1alpha1.SchemeGroupVersion.WithKind(kind)
}

func IsImmutable(secret *corev1.Secret) bool { return secret.Immutable != nil && *secret.Immutable }

func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func StringMapKeys(in map[string]string) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func controllerAnnotations(in map[string]string) map[string]string {
	out := map[string]string{}
	for _, key := range trackingAnnotations {
		if value, ok := in[key]; ok {
			out[key] = value
		}
	}
	return out
}

func secretSemanticallyEqual(a, b *corev1.Secret) bool {
	return a.Type == b.Type && equalBoolPtr(a.Immutable, b.Immutable) && reflect.DeepEqual(a.Data, b.Data) &&
		reflect.DeepEqual(a.Labels, b.Labels) && reflect.DeepEqual(a.Annotations, b.Annotations) &&
		reflect.DeepEqual(a.OwnerReferences, b.OwnerReferences)
}

func equalBoolPtr(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func mustJSON(v any) []byte        { b, _ := json.Marshal(v); return b }
func sortedUnique(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
func canonicalList(in []string) bool { return reflect.DeepEqual(in, sortedUnique(in)) }
func stringSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

func decodeValue(value []byte, encoding string, allowRawPadding bool) ([]byte, error) {
	if encoding == "raw" {
		return value, nil
	}
	var decoded []byte
	var err error
	switch encoding {
	case "base64":
		decoded, err = base64.StdEncoding.DecodeString(string(value))
	case "base64url":
		decoded, err = base64.URLEncoding.DecodeString(string(value))
	case "base32":
		decoded, err = base32.StdEncoding.DecodeString(string(value))
	case "hex":
		decoded, err = hex.DecodeString(string(value))
	default:
		return nil, fmt.Errorf("unsupported encoding")
	}
	if err != nil && allowRawPadding {
		// Character-length values are prefixes of canonical encodings and may omit padding.
		switch encoding {
		case "base64":
			decoded, err = base64.RawStdEncoding.DecodeString(string(value))
		case "base64url":
			decoded, err = base64.RawURLEncoding.DecodeString(string(value))
		case "base32":
			decoded, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(string(value))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("generated value is not canonical %s", encoding)
	}
	// Re-encoding is intentionally not required for character-length prefixes.
	return decoded, nil
}
