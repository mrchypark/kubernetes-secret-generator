package crd

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	controllerobservability "github.com/mittwald/kubernetes-secret-generator/pkg/controller/observability"
)

func TestNewSecretContracts(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	owner := &v1alpha1.StringSecret{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "StringSecret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "generated", Namespace: "default", UID: "owner-uid", Labels: map[string]string{"app": "example"},
		},
	}
	values := map[string][]byte{"value": []byte("secret")}
	secret, err := NewSecretWithScheme(owner, values, "", scheme)
	require.NoError(t, err)
	require.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	require.True(t, reflect.DeepEqual(values, secret.Data))
	require.Equal(t, owner.Name, secret.Name)
	require.Equal(t, owner.Namespace, secret.Namespace)
	require.Equal(t, owner.Labels, secret.Labels)
	require.True(t, ExactControllerOwner(secret, owner, ControllerGVK("StringSecret")))
	secret.Labels["app"] = "copy"
	require.Equal(t, "example", owner.Labels["app"])

	secret, err = NewSecretWithScheme(owner, values, string(corev1.SecretTypeTLS), scheme)
	require.NoError(t, err)
	require.Equal(t, corev1.SecretTypeTLS, secret.Type)

	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "default", UID: "uid"}}
	secret, err = NewSecret(configMap, values, "")
	require.NoError(t, err)
	require.Equal(t, "config", secret.Name)

	_, err = NewSecretWithScheme(owner, values, "", runtime.NewScheme())
	require.Error(t, err)
}

func TestCheckError(t *testing.T) {
	_, err := CheckError(apierrors.NewNotFound(schema.GroupResource{Group: "example.test", Resource: "things"}, "missing"))
	require.NoError(t, err)
	want := errors.New("boom")
	_, err = CheckError(want)
	require.ErrorIs(t, err, want)
}

func TestResourceChangePredicateLifecycle(t *testing.T) {
	predicates := []struct {
		name      string
		predicate interface {
			Create(event.TypedCreateEvent[client.Object]) bool
			Update(event.TypedUpdateEvent[client.Object]) bool
			Delete(event.TypedDeleteEvent[client.Object]) bool
		}
	}{
		{name: "without metrics", predicate: IgnoreStatusUpdatePredicate[client.Object]()},
		{name: "with metrics", predicate: ResourceChangePredicateFor[client.Object](controllerobservability.ControllerStringSecret)},
	}
	for _, tt := range predicates {
		t.Run(tt.name, func(t *testing.T) {
			old := &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", Generation: 1}}
			current := old.DeepCopy()
			current.Generation++
			require.True(t, tt.predicate.Create(event.TypedCreateEvent[client.Object]{Object: old}))
			require.True(t, tt.predicate.Update(event.TypedUpdateEvent[client.Object]{ObjectOld: old, ObjectNew: current}))
			require.True(t, tt.predicate.Delete(event.TypedDeleteEvent[client.Object]{Object: current}))
			require.False(t, tt.predicate.Delete(event.TypedDeleteEvent[client.Object]{Object: current, DeleteStateUnknown: true}))
		})
	}
}

func TestSecretChangePredicateObservesUniqueValidOwners(t *testing.T) {
	controller, notController := true, false
	gvk := ControllerGVK("StringSecret")
	valid := metav1.OwnerReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: "owner", UID: "uid", Controller: &controller}
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "generated", Namespace: "default", OwnerReferences: []metav1.OwnerReference{
		valid,
		valid,
		{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: "ignored", UID: "ignored", Controller: &notController},
	}}}
	current := old.DeepCopy()
	current.Data = map[string][]byte{"value": []byte("changed")}
	predicate := SecretChangePredicateFor(controllerobservability.ControllerStringSecret, gvk)
	require.True(t, predicate.Create(event.TypedCreateEvent[*corev1.Secret]{Object: current}))
	require.True(t, predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: old, ObjectNew: current}))
	require.True(t, predicate.Delete(event.TypedDeleteEvent[*corev1.Secret]{Object: current}))
}

func TestLoadTrackingRejectsMalformedBundles(t *testing.T) {
	valid := &corev1.Secret{Data: map[string][]byte{"a": []byte("value")}}
	tracking := &Tracking{DataKeys: []string{"a"}, LabelKeys: []string{"app"}, Fingerprints: map[string]string{"field": Fingerprint([]byte("field"))}}
	RefreshChecksums(tracking, valid.Data)
	StoreTracking(valid, tracking)

	tests := []struct {
		name  string
		state TrackingState
		edit  func(map[string]string)
	}{
		{name: "unsupported version", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationTrackingVersion] = "v2" }},
		{name: "malformed data keys", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedDataKeys] = "{" }},
		{name: "noncanonical data keys", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedDataKeys] = `["z","a"]` }},
		{name: "malformed label keys", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedLabelKeys] = "{" }},
		{name: "noncanonical label keys", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedLabelKeys] = `["z","a"]` }},
		{name: "wrong key digest", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedKeysDigest] = Fingerprint([]byte("wrong")) }},
		{name: "malformed fingerprints", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationGenerationFingerprint] = "null" }},
		{name: "empty fingerprint key", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationGenerationFingerprint] = `{"":"` + Fingerprint(nil) + `"}` }},
		{name: "invalid fingerprint", state: TrackingConflict, edit: func(a map[string]string) { a[AnnotationGenerationFingerprint] = `{"field":"bad"}` }},
		{name: "malformed checksums", state: TrackingChecksumIncomplete, edit: func(a map[string]string) { a[AnnotationManagedDataChecksums] = "null" }},
		{name: "empty checksum key", state: TrackingChecksumIncomplete, edit: func(a map[string]string) { a[AnnotationManagedDataChecksums] = `{"":"` + Fingerprint(nil) + `"}` }},
		{name: "invalid checksum", state: TrackingChecksumIncomplete, edit: func(a map[string]string) { a[AnnotationManagedDataChecksums] = `{"a":"bad"}` }},
		{name: "missing checksum", state: TrackingChecksumIncomplete, edit: func(a map[string]string) { a[AnnotationManagedDataChecksums] = `{}` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := valid.DeepCopy()
			tt.edit(secret.Annotations)
			_, state, err := LoadTracking(secret)
			require.Error(t, err)
			require.Equal(t, tt.state, state)
		})
	}

	_, state, err := LoadTracking(&corev1.Secret{})
	require.NoError(t, err)
	require.Equal(t, TrackingAbsent, state)
}

func TestTrackingAndManagedValueHelpers(t *testing.T) {
	require.NoError(t, RequireFingerprints(nil, "field"))
	tracking := &Tracking{DataKeys: []string{"b", "a"}, LabelKeys: []string{"z", "a"}, Fingerprints: map[string]string{"field": Fingerprint(nil)}}
	require.NoError(t, RequireFingerprints(tracking, "field"))
	require.Error(t, RequireFingerprints(tracking, "missing"))

	secret := &corev1.Secret{}
	ReserveChecksums(tracking)
	StoreTracking(secret, tracking)
	require.Equal(t, []string{"a", "b"}, tracking.DataKeys)
	require.Equal(t, []string{"a", "z"}, tracking.LabelKeys)
	require.Len(t, tracking.Checksums, 2)
	require.NotNil(t, secret.Annotations)
	markerSecret := &corev1.Secret{}
	SetRegenerationMarker(markerSecret, 4)
	require.Equal(t, "4", markerSecret.Annotations[AnnotationLastRegenerated])

	secret.Data = map[string][]byte{"removed": []byte("old"), "kept": []byte("old"), "user": []byte("user")}
	ApplyManagedData(secret, map[string]string{"kept": "new", "added": "value"}, []string{"kept", "added"}, []string{"removed", "kept"})
	require.True(t, reflect.DeepEqual(map[string][]byte{"kept": []byte("new"), "added": []byte("value"), "user": []byte("user")}, secret.Data))
	ApplyManagedData(&corev1.Secret{}, map[string]string{"new": "value"}, []string{"new"}, nil)

	secret.Labels = map[string]string{"removed": "old", "kept": "old", "user": "user"}
	ApplyManagedLabels(secret, map[string]string{"kept": "new", "added": "value"}, []string{"removed", "kept"})
	require.Equal(t, map[string]string{"kept": "new", "added": "value", "user": "user"}, secret.Labels)
	empty := &corev1.Secret{}
	ApplyManagedLabels(empty, nil, nil)
	require.Nil(t, empty.Labels)
	ApplyManagedLabels(empty, map[string]string{"new": "value"}, nil)
	require.Equal(t, map[string]string{"new": "value"}, empty.Labels)

	require.True(t, SameStrings([]string{"b", "a"}, []string{"a", "b"}))
	require.False(t, SameStrings([]string{"a"}, []string{"b"}))
	require.Equal(t, []string{"a", "b"}, StringMapKeys(map[string]string{"b": "2", "a": "1"}))
}

func TestValidateGeneratedValue(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		length     int
		byteLength bool
		encoding   string
		wantErr    bool
	}{
		{name: "empty", length: 1, encoding: "raw", wantErr: true},
		{name: "character length mismatch", value: "abc", length: 2, encoding: "raw", wantErr: true},
		{name: "raw characters", value: "a!?", length: 3, encoding: "raw"},
		{name: "base64 characters", value: "A+/9", length: 4, encoding: "base64"},
		{name: "base64url characters", value: "A-_9", length: 4, encoding: "base64url"},
		{name: "base32 characters", value: "ABC7", length: 4, encoding: "base32"},
		{name: "hex characters", value: "09af", length: 4, encoding: "hex"},
		{name: "invalid character", value: "ABC=", length: 4, encoding: "base32", wantErr: true},
		{name: "bytes base64", value: "YWI=", length: 2, byteLength: true, encoding: "base64"},
		{name: "bytes base64url", value: "YWI=", length: 2, byteLength: true, encoding: "base64url"},
		{name: "bytes base32", value: "MFRQ====", length: 2, byteLength: true, encoding: "base32"},
		{name: "bytes hex", value: "6162", length: 2, byteLength: true, encoding: "hex"},
		{name: "bytes raw", value: "ab", length: 2, byteLength: true, encoding: "raw"},
		{name: "bytes wrong length", value: "YWI=", length: 3, byteLength: true, encoding: "base64", wantErr: true},
		{name: "bytes malformed", value: "%%%", length: 2, byteLength: true, encoding: "base64", wantErr: true},
		{name: "unsupported", value: "abc", length: 3, byteLength: true, encoding: "unknown", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGeneratedValue([]byte(tt.value), tt.length, tt.byteLength, tt.encoding)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	for _, tt := range []struct {
		encoding string
		value    string
	}{
		{encoding: "base64", value: "YWI"},
		{encoding: "base64url", value: "YWI"},
		{encoding: "base32", value: "MFRA"},
	} {
		decoded, err := decodeValue([]byte(tt.value), tt.encoding, true)
		require.NoError(t, err)
		require.True(t, bytes.Equal([]byte("ab"), decoded))
	}
	_, err := decodeValue([]byte("%%%"), "base64", true)
	require.Error(t, err)
}

func TestPatchSecretLifecycle(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "generated", Namespace: "default"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"value": []byte("old")}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	stored := &corev1.Secret{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(existing), stored))

	wrote, err := (&Client{Client: kube}).PatchSecret(t.Context(), stored, stored.DeepCopy())
	require.NoError(t, err)
	require.False(t, wrote)

	desired := stored.DeepCopy()
	desired.Data["value"] = []byte("new")
	wrote, err = (&Client{Client: kube}).PatchSecret(t.Context(), stored, desired)
	require.NoError(t, err)
	require.True(t, wrote)
	updated := &corev1.Secret{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(existing), updated))
	require.True(t, bytes.Equal([]byte("new"), updated.Data["value"]))

	missing := existing.DeepCopy()
	missing.Name = "missing"
	missingDesired := missing.DeepCopy()
	missingDesired.Data["value"] = []byte("new")
	wrote, err = (&Client{Client: kube}).PatchSecret(t.Context(), missing, missingDesired)
	require.Error(t, err)
	require.True(t, wrote)
}

func TestStatusLifecycleHelpers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "StringSecret"},
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "owner", Generation: 3},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "generated", Namespace: "default", UID: "secret"}}

	_, err := ReadyStatus(t.Context(), kube, instance, secret, 2)
	require.NoError(t, err)
	persisted := &v1alpha1.StringSecret{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(instance), persisted))
	require.Equal(t, metav1.ConditionTrue, persisted.Status.Conditions[0].Status)
	require.EqualValues(t, 3, persisted.Status.ObservedGeneration)
	require.EqualValues(t, 2, persisted.Status.LastRegeneratedGeneration)
	require.Equal(t, secret.Name, persisted.Status.Secret.Name)

	_, err = TerminalStatus(t.Context(), kube, persisted, secret, v1alpha1.ReasonInvalidSpec, "invalid", true, -1)
	require.NoError(t, err)
	persisted = &v1alpha1.StringSecret{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(instance), persisted))
	require.Equal(t, metav1.ConditionFalse, persisted.Status.Conditions[0].Status)
	require.Equal(t, v1alpha1.ReasonInvalidSpec, persisted.Status.Conditions[0].Reason)

	applyErr := errors.New("apply failed")
	_, err = ApplyFailure(t.Context(), kube, persisted, nil, applyErr)
	require.ErrorIs(t, err, applyErr)
	persisted = &v1alpha1.StringSecret{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(instance), persisted))
	require.Equal(t, v1alpha1.ReasonApplyFailed, persisted.Status.Conditions[0].Reason)

	missing := persisted.DeepCopy()
	missing.Name = "missing"
	_, err = ReadyStatus(t.Context(), kube, missing, nil, 0)
	require.Error(t, err)
	_, err = TerminalStatus(t.Context(), kube, missing, nil, v1alpha1.ReasonInvalidSpec, "invalid", false, -1)
	require.Error(t, err)
	_, err = ApplyFailure(t.Context(), kube, missing, nil, applyErr)
	require.ErrorIs(t, err, applyErr)
}

func TestMetadataAndSemanticHelpers(t *testing.T) {
	require.Equal(t, v1alpha1.SchemeGroupVersion.WithKind("BasicAuth"), ControllerGVK("BasicAuth"))
	require.False(t, IsImmutable(&corev1.Secret{}))
	yes, no := true, false
	require.True(t, IsImmutable(&corev1.Secret{Immutable: &yes}))
	require.False(t, IsImmutable(&corev1.Secret{Immutable: &no}))
	require.Nil(t, cloneStrings(nil))
	require.Equal(t, map[string]string{"a": "b"}, cloneStrings(map[string]string{"a": "b"}))

	base := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          map[string]string{"app": "example"},
			Annotations:     map[string]string{"annotation": "value"},
			OwnerReferences: []metav1.OwnerReference{{Name: "owner", UID: types.UID("uid")}},
		},
		Type: corev1.SecretTypeOpaque, Immutable: &yes, Data: map[string][]byte{"value": []byte("secret")},
	}
	require.True(t, secretSemanticallyEqual(base, base.DeepCopy()))
	mutations := []func(*corev1.Secret){
		func(s *corev1.Secret) { s.Type = corev1.SecretTypeTLS },
		func(s *corev1.Secret) { s.Immutable = &no },
		func(s *corev1.Secret) { s.Data["value"] = []byte("changed") },
		func(s *corev1.Secret) { s.Labels["app"] = "changed" },
		func(s *corev1.Secret) { s.Annotations["annotation"] = "changed" },
		func(s *corev1.Secret) { s.OwnerReferences = nil },
	}
	for _, mutate := range mutations {
		other := base.DeepCopy()
		mutate(other)
		require.False(t, secretSemanticallyEqual(base, other))
	}
	require.True(t, equalBoolPtr(nil, nil))
	require.False(t, equalBoolPtr(nil, &yes))
	require.True(t, equalBoolPtr(&yes, &yes))
}
