package crd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestExactControllerOwner(t *testing.T) {
	controller, notController := true, false
	owner := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: types.UID("uid")}}
	gvk := schema.GroupVersionKind{Group: "secretgenerator.mittwald.de", Version: "v1alpha1", Kind: "StringSecret"}
	valid := metav1.OwnerReference{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Name: owner.Name, UID: owner.UID, Controller: &controller}
	tests := []struct {
		name string
		ref  metav1.OwnerReference
		want bool
	}{
		{name: "exact", ref: valid, want: true},
		{name: "wrong api version", ref: mutateOwner(valid, func(r *metav1.OwnerReference) { r.APIVersion = "other/v1" })},
		{name: "wrong kind", ref: mutateOwner(valid, func(r *metav1.OwnerReference) { r.Kind = "BasicAuth" })},
		{name: "wrong name", ref: mutateOwner(valid, func(r *metav1.OwnerReference) { r.Name = "other" })},
		{name: "wrong uid", ref: mutateOwner(valid, func(r *metav1.OwnerReference) { r.UID = "other" })},
		{name: "not controller", ref: mutateOwner(valid, func(r *metav1.OwnerReference) { r.Controller = &notController })},
		{name: "nil controller", ref: mutateOwner(valid, func(r *metav1.OwnerReference) { r.Controller = nil })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{tt.ref}}}
			require.Equal(t, tt.want, ExactControllerOwner(secret, owner, gvk))
		})
	}
	require.False(t, ExactControllerOwner(&corev1.Secret{}, owner, gvk))
	require.False(t, ExactControllerOwner(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
		valid,
		{APIVersion: "example.test/v1", Kind: "Observer", Name: "other", UID: "other"},
	}}}, owner, gvk))
}

func TestTrackingStateMachine(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}, Data: map[string][]byte{"literal": []byte("value")}}
	tracking := &Tracking{DataKeys: []string{"literal"}, LabelKeys: []string{"app"}, Checksums: map[string]string{}, Fingerprints: map[string]string{"field": Fingerprint([]byte("spec"))}}
	RefreshChecksums(tracking, secret.Data)
	StoreTracking(secret, tracking)

	got, state, err := LoadTracking(secret)
	require.NoError(t, err)
	require.Equal(t, TrackingValid, state)
	require.Equal(t, tracking, got)

	partial := secret.DeepCopy()
	delete(partial.Annotations, AnnotationManagedDataKeys)
	_, state, err = LoadTracking(partial)
	require.Error(t, err)
	require.Equal(t, TrackingConflict, state)

	checksumLoss := secret.DeepCopy()
	checksumLoss.Annotations[AnnotationManagedDataChecksums] = "{}"
	got, state, err = LoadTracking(checksumLoss)
	require.Error(t, err)
	require.Equal(t, TrackingChecksumIncomplete, state)
	require.Empty(t, got.Checksums)

	malformedChecksum := secret.DeepCopy()
	malformedChecksum.Annotations[AnnotationManagedDataChecksums] = `{"literal":"bad"}`
	_, state, err = LoadTracking(malformedChecksum)
	require.Error(t, err)
	require.Equal(t, TrackingChecksumIncomplete, state)
}

func TestClassifyTrackingCrossProduct(t *testing.T) {
	tests := []struct {
		name        string
		initialized bool
		state       TrackingState
		want        TrackingAction
	}{
		{name: "false absent baselines", state: TrackingAbsent, want: TrackingBaseline},
		{name: "false valid reconstructs status", state: TrackingValid, want: TrackingReconstructStatus},
		{name: "false incomplete rejects", state: TrackingChecksumIncomplete, want: TrackingReject},
		{name: "false conflict rejects", state: TrackingConflict, want: TrackingReject},
		{name: "true absent rejects", initialized: true, state: TrackingAbsent, want: TrackingReject},
		{name: "true valid reconciles", initialized: true, state: TrackingValid, want: TrackingReconcile},
		{name: "true incomplete repairs", initialized: true, state: TrackingChecksumIncomplete, want: TrackingRepairChecksums},
		{name: "true conflict rejects", initialized: true, state: TrackingConflict, want: TrackingReject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ClassifyTracking(tt.initialized, tt.state))
		})
	}
}

func TestRegenerationMarker(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	marker, err := ParseRegenerationMarker(secret, 3)
	require.NoError(t, err)
	require.EqualValues(t, -1, marker)
	SetRegenerationMarker(secret, 3)
	marker, err = ParseRegenerationMarker(secret, 3)
	require.NoError(t, err)
	require.EqualValues(t, 3, marker)
	secret.Annotations[AnnotationLastRegenerated] = "4"
	_, err = ParseRegenerationMarker(secret, 3)
	require.Error(t, err)
}

func TestRotationIntervalAndSchedule(t *testing.T) {
	for _, value := range []string{"", "1m", "90m", "8760h"} {
		_, err := ParseRotationInterval(value)
		require.NoError(t, err, value)
	}
	for _, value := range []string{"bad", "0s", "-1m", "59s", "8761h"} {
		_, err := ParseRotationInterval(value)
		require.Error(t, err, value)
	}

	now := time.Date(2026, 7, 12, 12, 0, 0, 123, time.FixedZone("test", 9*60*60))
	secret := &corev1.Secret{}
	due, after, anchored, err := RotationSchedule(secret, time.Hour, now)
	require.NoError(t, err)
	require.False(t, due)
	require.False(t, anchored)
	require.Equal(t, time.Hour, after)

	SetRotationAnchor(secret, now)
	require.Equal(t, now.UTC().Format(time.RFC3339Nano), secret.Annotations[AnnotationRotationAnchor])
	for _, tt := range []struct {
		name      string
		interval  time.Duration
		now       time.Time
		wantDue   bool
		wantAfter time.Duration
	}{
		{name: "same interval", interval: time.Hour, now: now.Add(15 * time.Minute), wantAfter: 45 * time.Minute},
		{name: "shorter interval is due", interval: 10 * time.Minute, now: now.Add(15 * time.Minute), wantDue: true},
		{name: "longer interval reuses anchor", interval: 2 * time.Hour, now: now.Add(15 * time.Minute), wantAfter: 105 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			due, after, anchored, err := RotationSchedule(secret, tt.interval, tt.now)
			require.NoError(t, err)
			require.Equal(t, tt.wantDue, due)
			require.True(t, anchored)
			require.Equal(t, tt.wantAfter, after)
		})
	}

	due, after, anchored, err = RotationSchedule(secret, 0, now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, due)
	require.False(t, anchored)
	require.Zero(t, after)

	secret.Annotations[AnnotationRotationAnchor] = "invalid"
	_, _, _, err = RotationSchedule(secret, time.Hour, now)
	require.Error(t, err)
	ClearRotationAnchor(secret)
	require.NotContains(t, secret.Annotations, AnnotationRotationAnchor)
}

func TestPatchSecretRejectsTypeMutationBeforeClientCall(t *testing.T) {
	existing := &corev1.Secret{Type: corev1.SecretTypeOpaque}
	desired := existing.DeepCopy()
	desired.Type = corev1.SecretTypeTLS
	wrote, err := (&Client{}).PatchSecret(t.Context(), existing, desired)
	require.Error(t, err)
	require.False(t, wrote)
}

func TestEffectiveSecretType(t *testing.T) {
	require.Equal(t, corev1.SecretTypeOpaque, EffectiveSecretType(""))
	require.True(t, SecretTypeMatches(&corev1.Secret{}, corev1.SecretTypeOpaque))
	require.True(t, SecretTypeMatches(&corev1.Secret{Type: corev1.SecretTypeOpaque}, corev1.SecretTypeOpaque))
	require.False(t, SecretTypeMatches(&corev1.Secret{Type: corev1.SecretTypeTLS}, corev1.SecretTypeOpaque))
}

func TestResourceChangePredicateIncludesLabels(t *testing.T) {
	predicate := ResourceChangePredicateFor[client.Object]("")
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Generation: 1, Labels: map[string]string{"managed": "one"}}}
	for _, current := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Generation: 1, Labels: map[string]string{"managed": "two"}}},
		{ObjectMeta: metav1.ObjectMeta{Generation: 1}},
	} {
		require.True(t, predicate.Update(event.TypedUpdateEvent[client.Object]{ObjectOld: old, ObjectNew: current}))
	}
	unchanged := old.DeepCopy()
	unchanged.ResourceVersion = "2"
	require.False(t, predicate.Update(event.TypedUpdateEvent[client.Object]{ObjectOld: old, ObjectNew: unchanged}))
}

func TestSecretChangePredicate(t *testing.T) {
	controller := true
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Annotations:     map[string]string{"user": "old"},
		Labels:          map[string]string{"managed": "one"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "example.test/v1", Kind: "Owner", Name: "owner", UID: "uid", Controller: &controller}},
	}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"key": []byte("value")}}
	predicate := SecretChangePredicate()
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
		want   bool
	}{
		{name: "resource version only", mutate: func(s *corev1.Secret) { s.ResourceVersion = "2" }},
		{name: "unrelated annotation", mutate: func(s *corev1.Secret) { s.Annotations["user"] = "new" }},
		{name: "data", mutate: func(s *corev1.Secret) { s.Data["key"] = []byte("changed") }, want: true},
		{name: "type", mutate: func(s *corev1.Secret) { s.Type = corev1.SecretTypeTLS }, want: true},
		{name: "immutable", mutate: func(s *corev1.Secret) { immutable := true; s.Immutable = &immutable }, want: true},
		{name: "labels", mutate: func(s *corev1.Secret) { s.Labels["managed"] = "two" }, want: true},
		{name: "owner references", mutate: func(s *corev1.Secret) { s.OwnerReferences = nil }, want: true},
	}
	for _, annotation := range trackingAnnotations {
		annotation := annotation
		tests = append(tests, struct {
			name   string
			mutate func(*corev1.Secret)
			want   bool
		}{name: annotation, mutate: func(s *corev1.Secret) { s.Annotations[annotation] = "changed" }, want: true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := old.DeepCopy()
			tt.mutate(current)
			require.Equal(t, tt.want, predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: old, ObjectNew: current}))
		})
	}
	require.True(t, predicate.Delete(event.TypedDeleteEvent[*corev1.Secret]{Object: old}))
}

func mutateOwner(in metav1.OwnerReference, mutate func(*metav1.OwnerReference)) metav1.OwnerReference {
	mutate(&in)
	return in
}
