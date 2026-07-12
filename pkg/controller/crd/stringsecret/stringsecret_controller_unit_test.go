package stringsecret

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
	controllerobservability "github.com/mittwald/kubernetes-secret-generator/pkg/controller/observability"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
)

func TestReconcileConvergesAndSelfHeals(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: types.UID("owner"), Generation: 1, Labels: map[string]string{"managed": "one"}},
		Spec:       v1alpha1.StringSecretSpec{Data: map[string]string{"literal": "desired"}, Fields: []v1alpha1.Field{{FieldName: "first", Length: "24", Encoding: "base64"}, {FieldName: "second", Length: "24", Encoding: "base64"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}

	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	created := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, created))
	first, second := append([]byte(nil), created.Data["first"]...), append([]byte(nil), created.Data["second"]...)
	require.True(t, string(created.Data["literal"]) == "desired")
	require.NotEmpty(t, created.Annotations)

	created.Data["literal"] = []byte("drift")
	created.Data["first"] = []byte("drift")
	created.Data["unmanaged"] = []byte("preserved")
	created.Labels["unmanaged"] = "preserved"
	require.NoError(t, cl.Update(context.Background(), created))
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	repaired := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, repaired))
	require.True(t, string(repaired.Data["literal"]) == "desired")
	require.False(t, bytes.Equal(first, repaired.Data["first"]))
	require.True(t, bytes.Equal(second, repaired.Data["second"]))
	require.True(t, string(repaired.Data["unmanaged"]) == "preserved")
	require.Equal(t, "preserved", repaired.Labels["unmanaged"])

	require.NoError(t, cl.Delete(context.Background(), repaired))
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	recreated := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, recreated))
	require.False(t, bytes.Equal(repaired.Data["first"], recreated.Data["first"]))
	_, exists := recreated.Data["unmanaged"]
	require.False(t, exists)
}

func TestStringSecretRotationInterval(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "scheduled-string", Namespace: "default", UID: "owner", Generation: 1},
		Spec:       v1alpha1.StringSecretSpec{Data: map[string]string{"literal": "stable"}, Fields: []v1alpha1.Field{{FieldName: "one", Length: "24", Encoding: "base64"}, {FieldName: "two", Length: "24", Encoding: "base64"}}, RotationInterval: "1h"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme, clock: func() time.Time { return now }}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)}
	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, time.Hour, result.RequeueAfter)
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	one, two := append([]byte(nil), managed.Data["one"]...), append([]byte(nil), managed.Data["two"]...)

	now = now.Add(time.Hour)
	result, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, time.Hour, result.RequeueAfter)
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.NotEqual(t, one, managed.Data["one"])
	require.NotEqual(t, two, managed.Data["two"])
	require.Equal(t, "stable", string(managed.Data["literal"]))
	require.Equal(t, "1", managed.Annotations[crd.AnnotationLastRegenerated])
	after := append([]byte(nil), managed.Data["one"]...)
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.Equal(t, after, managed.Data["one"])
}

func TestStringSecretRotationIntervalRequiresGeneratedField(t *testing.T) {
	_, err := stringPlanFor(&v1alpha1.StringSecret{Spec: v1alpha1.StringSecretSpec{Data: map[string]string{"literal": "value"}, RotationInterval: "1h"}})
	require.Error(t, err)
}

func TestJointDataAndChecksumTamperIsNotTrusted(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "joint-tamper", Namespace: "default", UID: "owner", Generation: 1},
		Spec:       v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	old := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, old))
	old.UID = "secret-uid"
	require.NoError(t, cl.Update(context.Background(), old))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, old))
	dataOnly := old.DeepCopy()
	dataOnly.Data["value"] = []byte("tampered")
	current := dataOnly.DeepCopy()
	checksums := map[string]string{}
	require.NoError(t, json.Unmarshal([]byte(current.Annotations[crd.AnnotationManagedDataChecksums]), &checksums))
	checksums["value"] = crd.DataChecksum("value", current.Data["value"])
	encoded, err := json.Marshal(checksums)
	require.NoError(t, err)
	current.Annotations[crd.AnnotationManagedDataChecksums] = string(encoded)
	require.NoError(t, cl.Update(context.Background(), current))
	predicate := crd.SecretChangePredicateFor(controllerobservability.ControllerStringSecret, crd.ControllerGVK(Kind))
	require.True(t, predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: old, ObjectNew: dataOnly}))
	require.True(t, predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: dataOnly, ObjectNew: current}))
	tamperedVersion := current.ResourceVersion
	require.NoError(t, reconcileOnce(r, request))
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.Equal(t, tamperedVersion, after.ResourceVersion)
	require.True(t, bytes.Equal([]byte("tampered"), after.Data["value"]))
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, v1alpha1.ReasonTrackingStateConflict, updated.Status.Conditions[0].Reason)
}

func TestOwnershipConflictIsTerminalAndDoesNotWriteSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	controller := true
	instance := &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "string-owner", Namespace: "default", UID: "current-uid", Generation: 3}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "0", Encoding: "base64"}}}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace, Labels: map[string]string{"preserved": "true"}, OwnerReferences: []metav1.OwnerReference{{
		APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind, Name: instance.Name, UID: "old-uid", Controller: &controller,
	}}}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"preserved": []byte("sentinel")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, foreign).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}

	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Zero(t, result)
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.True(t, reflect.DeepEqual(foreign, after))
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, int64(3), updated.Status.ObservedGeneration)
	require.Equal(t, v1alpha1.ReasonSecretOwnershipConflict, updated.Status.Conditions[0].Reason)
	statusVersion := updated.ResourceVersion
	require.NoError(t, reconcileOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, statusVersion, updated.ResourceVersion)
}

func TestForceRegenerateRunsOncePerGeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "force", Namespace: "default", UID: "owner", Generation: 7}, Spec: v1alpha1.StringSecretSpec{ForceRegenerate: true, Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	first := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, first))
	require.NoError(t, reconcileOnce(r, request))
	second := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, second))
	require.True(t, bytes.Equal(first.Data["value"], second.Data["value"]))
	require.Equal(t, first.ResourceVersion, second.ResourceVersion)
	require.Equal(t, "7", second.Annotations["secretgenerator.mittwald.de/last-regenerated-generation"])
	statusAfterFirst := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, statusAfterFirst))
	require.Len(t, statusAfterFirst.Status.Conditions, 1)
	require.Equal(t, metav1.ConditionTrue, statusAfterFirst.Status.Conditions[0].Status)
	statusVersion := statusAfterFirst.ResourceVersion
	require.NoError(t, reconcileOnce(r, request))
	statusAfterSecond := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, statusAfterSecond))
	require.Equal(t, statusVersion, statusAfterSecond.ResourceVersion)
}

func TestStatusWriteFailureDoesNotRepeatForceRegeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "force-status-failure", Namespace: "default", UID: "owner", Generation: 7}, Spec: v1alpha1.StringSecretSpec{ForceRegenerate: true, Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	cl := &failStatusOnceClient{Client: base, failures: 1}
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	_, err := r.Reconcile(context.Background(), request)
	require.Error(t, err)
	created := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), request.NamespacedName, created))
	value := append([]byte(nil), created.Data["value"]...)
	version := created.ResourceVersion
	require.Equal(t, "7", created.Annotations[crd.AnnotationLastRegenerated])
	require.NoError(t, reconcileOnce(r, request))
	after := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), request.NamespacedName, after))
	require.True(t, bytes.Equal(value, after.Data["value"]))
	require.Equal(t, version, after.ResourceVersion)
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, base.Get(context.Background(), request.NamespacedName, updated))
	require.True(t, updated.Status.TrackingInitialized)
}

func TestMalformedRegenerationMarkerIsTerminalAndDoesNotWriteSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "malformed-marker", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	managed.Annotations[crd.AnnotationLastRegenerated] = "malformed"
	require.NoError(t, cl.Update(context.Background(), managed))
	mutatedVersion := managed.ResourceVersion
	require.NoError(t, reconcileOnce(r, request))
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.Equal(t, mutatedVersion, after.ResourceVersion)
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, v1alpha1.ReasonRegenerationStateConflict, updated.Status.Conditions[0].Reason)
}

func TestTrackingStatusReconstructionDoesNotWriteSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "status-reconstruction", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	secretVersion := managed.ResourceVersion
	rolledBack := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, rolledBack))
	rolledBack.Status = v1alpha1.StringSecretStatus{}
	require.NoError(t, cl.Status().Update(context.Background(), rolledBack))
	require.NoError(t, reconcileOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.Equal(t, secretVersion, managed.ResourceVersion)
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, rolledBack))
	require.True(t, rolledBack.Status.TrackingInitialized)
	require.Equal(t, v1alpha1.ReasonReconciled, rolledBack.Status.Conditions[0].Reason)
}

func TestTrackingStatusReconstructionAlsoConvergesChangedSpec(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "status-and-spec", Namespace: "default", UID: "owner", Generation: 1, Labels: map[string]string{"managed": "one"}}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	rolledBack := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, rolledBack))
	rolledBack.Spec.Fields[0].Length = "30"
	rolledBack.Labels["managed"] = "two"
	rolledBack.Generation = 2
	rolledBack.Status = v1alpha1.StringSecretStatus{}
	require.NoError(t, cl.Update(context.Background(), rolledBack))
	require.NoError(t, cl.Status().Update(context.Background(), rolledBack))
	require.NoError(t, reconcileOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.Len(t, managed.Data["value"], 30)
	require.Equal(t, "two", managed.Labels["managed"])
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, rolledBack))
	require.True(t, rolledBack.Status.TrackingInitialized)
}

func TestInitializedStatusWithAbsentTrackingStaysTerminal(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "tracking-loss", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	for _, key := range []string{crd.AnnotationTrackingVersion, crd.AnnotationManagedDataKeys, crd.AnnotationManagedLabelKeys, crd.AnnotationManagedDataChecksums, crd.AnnotationGenerationFingerprint, crd.AnnotationManagedKeysDigest} {
		delete(managed.Annotations, key)
	}
	require.NoError(t, cl.Update(context.Background(), managed))
	mutatedVersion := managed.ResourceVersion
	for range 2 {
		require.NoError(t, reconcileOnce(r, request))
	}
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.Equal(t, mutatedVersion, after.ResourceVersion)
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.True(t, updated.Status.TrackingInitialized)
	require.Equal(t, v1alpha1.ReasonTrackingStateConflict, updated.Status.Conditions[0].Reason)
}

func TestInvalidSpecDoesNotEraseInitializedTrackingState(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "invalid-then-fixed", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	updated.Spec.Fields[0].Length = "0"
	updated.Generation = 2
	require.NoError(t, cl.Update(context.Background(), updated))
	require.NoError(t, reconcileOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.True(t, updated.Status.TrackingInitialized)
	require.Equal(t, v1alpha1.ReasonInvalidSpec, updated.Status.Conditions[0].Reason)
	updated.Spec.Fields[0].Length = "30"
	updated.Generation = 3
	require.NoError(t, cl.Update(context.Background(), updated))
	require.NoError(t, reconcileOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.Len(t, managed.Data["value"], 30)
}

func TestImmutableDriftIsTerminalAndDoesNotWriteSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "immutable", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	require.NoError(t, reconcileOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	immutable := true
	managed.Immutable = &immutable
	managed.Data["value"] = []byte("drift")
	require.NoError(t, cl.Update(context.Background(), managed))
	driftVersion := managed.ResourceVersion
	require.NoError(t, reconcileOnce(r, request))
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.Equal(t, driftVersion, after.ResourceVersion)
	require.True(t, bytes.Equal([]byte("drift"), after.Data["value"]))
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, v1alpha1.ReasonImmutableSecretConflict, updated.Status.Conditions[0].Reason)
}

func TestImmutableCurrentSecretRejectsInvalidRegenerationMarker(t *testing.T) {
	for _, marker := range []string{"malformed", "2"} {
		t.Run(marker, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
			instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "immutable-marker", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
			r := &ReconcileStringSecret{client: cl, scheme: scheme}
			request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
			require.NoError(t, reconcileOnce(r, request))
			managed := &corev1.Secret{}
			require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
			immutable := true
			managed.Immutable = &immutable
			managed.Annotations[crd.AnnotationLastRegenerated] = marker
			require.NoError(t, cl.Update(context.Background(), managed))
			version := managed.ResourceVersion
			require.NoError(t, reconcileOnce(r, request))
			after := &corev1.Secret{}
			require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
			require.Equal(t, version, after.ResourceVersion)
			updated := &v1alpha1.StringSecret{}
			require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
			require.Equal(t, v1alpha1.ReasonRegenerationStateConflict, updated.Status.Conditions[0].Reason)
		})
	}
}

func TestSecretTypeMismatchIsTerminalBeforeOtherState(t *testing.T) {
	for _, tt := range []struct {
		name           string
		removeTracking bool
		immutable      bool
		force          bool
	}{
		{name: "tracked"},
		{name: "tracking absent", removeTracking: true},
		{name: "immutable", immutable: true},
		{name: "force requested", force: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
			instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "type-mismatch", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Type: "example.test/one", Fields: []v1alpha1.Field{{FieldName: "value", Length: "24", Encoding: "base64"}}}}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
			r := &ReconcileStringSecret{client: cl, scheme: scheme}
			request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
			require.NoError(t, reconcileOnce(r, request))
			managed := &corev1.Secret{}
			require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
			managed.Type = "example.test/out-of-band"
			if tt.removeTracking {
				for _, key := range []string{crd.AnnotationTrackingVersion, crd.AnnotationManagedDataKeys, crd.AnnotationManagedLabelKeys, crd.AnnotationManagedDataChecksums, crd.AnnotationGenerationFingerprint, crd.AnnotationManagedKeysDigest} {
					delete(managed.Annotations, key)
				}
			}
			if tt.immutable {
				value := true
				managed.Immutable = &value
			}
			require.NoError(t, cl.Update(context.Background(), managed))
			secretVersion := managed.ResourceVersion
			if tt.force {
				updated := &v1alpha1.StringSecret{}
				require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
				updated.Spec.ForceRegenerate = true
				updated.Generation = 2
				require.NoError(t, cl.Update(context.Background(), updated))
			}
			require.NoError(t, reconcileOnce(r, request))
			after := &corev1.Secret{}
			require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
			require.Equal(t, secretVersion, after.ResourceVersion)
			updated := &v1alpha1.StringSecret{}
			require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
			require.Equal(t, v1alpha1.ReasonSecretTypeConflict, updated.Status.Conditions[0].Reason)
			require.True(t, updated.Status.TrackingInitialized)
			require.EqualValues(t, 1, updated.Status.LastRegeneratedGeneration)
			require.NotNil(t, updated.Status.Secret)
			statusVersion := updated.ResourceVersion
			require.NoError(t, reconcileOnce(r, request))
			require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
			require.Equal(t, statusVersion, updated.ResourceVersion)
		})
	}
}

func TestStringPlanValidationAndLegacyBaseline(t *testing.T) {
	instance := &v1alpha1.StringSecret{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}, Generation: 2},
		Spec: v1alpha1.StringSecretSpec{
			Data:   map[string]string{"literal": "value"},
			Fields: []v1alpha1.Field{{FieldName: "generated", Length: "6", Encoding: "raw"}},
		},
	}
	plan, err := stringPlanFor(instance)
	require.NoError(t, err)
	existing := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"literal": []byte("value"), "generated": []byte("123456")}}
	require.NoError(t, plan.validateLegacy(instance, existing))

	tests := []struct {
		name   string
		mutate func(*v1alpha1.StringSecret, *corev1.Secret)
	}{
		{name: "type", mutate: func(_ *v1alpha1.StringSecret, s *corev1.Secret) { s.Type = corev1.SecretTypeTLS }},
		{name: "literal", mutate: func(_ *v1alpha1.StringSecret, s *corev1.Secret) { s.Data["literal"] = []byte("drift") }},
		{name: "generated", mutate: func(_ *v1alpha1.StringSecret, s *corev1.Secret) { s.Data["generated"] = []byte("short") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copyInstance := instance.DeepCopy()
			copySecret := existing.DeepCopy()
			tt.mutate(copyInstance, copySecret)
			require.Error(t, plan.validateLegacy(copyInstance, copySecret))
		})
	}

	for _, spec := range []v1alpha1.StringSecretSpec{
		{Data: map[string]string{"same": "x"}, Fields: []v1alpha1.Field{{FieldName: "same", Length: "6", Encoding: "raw"}}},
		{Fields: []v1alpha1.Field{{FieldName: "value", Length: "bad", Encoding: "raw"}}},
		{Fields: []v1alpha1.Field{{FieldName: "value", Length: "6", Encoding: "bad"}}},
	} {
		_, err = stringPlanFor(&v1alpha1.StringSecret{Spec: spec})
		require.Error(t, err)
	}
}

func TestStringImmutableAndMarkerHelpers(t *testing.T) {
	instance := &v1alpha1.StringSecret{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}, Generation: 2},
		Spec:       v1alpha1.StringSecretSpec{Data: map[string]string{"literal": "value"}, Fields: []v1alpha1.Field{{FieldName: "generated", Length: "6", Encoding: "raw"}}},
	}
	plan, err := stringPlanFor(instance)
	require.NoError(t, err)
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}, Annotations: map[string]string{crd.AnnotationLastRegenerated: "2"}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"literal": []byte("value"), "generated": []byte("123456")}}
	tracking := plan.tracking(instance)
	crd.RefreshChecksums(tracking, existing.Data)
	require.True(t, plan.immutableCurrent(instance, existing, tracking))
	require.EqualValues(t, 2, markerOrUnset(existing, instance.Generation))

	tests := []struct {
		name   string
		mutate func(*v1alpha1.StringSecret, *corev1.Secret, *crd.Tracking)
	}{
		{name: "nil tracking", mutate: func(_ *v1alpha1.StringSecret, _ *corev1.Secret, tr *crd.Tracking) { *tr = crd.Tracking{} }},
		{name: "labels", mutate: func(_ *v1alpha1.StringSecret, s *corev1.Secret, _ *crd.Tracking) { s.Labels["managed"] = "no" }},
		{name: "removed label remains", mutate: func(i *v1alpha1.StringSecret, _ *corev1.Secret, _ *crd.Tracking) { delete(i.Labels, "managed") }},
		{name: "literal", mutate: func(_ *v1alpha1.StringSecret, s *corev1.Secret, _ *crd.Tracking) { s.Data["literal"] = []byte("drift") }},
		{name: "fingerprint", mutate: func(_ *v1alpha1.StringSecret, _ *corev1.Secret, tr *crd.Tracking) {
			tr.Fingerprints["generated"] = "bad"
		}},
		{name: "checksum", mutate: func(_ *v1alpha1.StringSecret, _ *corev1.Secret, tr *crd.Tracking) { tr.Checksums["generated"] = "bad" }},
		{name: "force", mutate: func(i *v1alpha1.StringSecret, s *corev1.Secret, _ *crd.Tracking) {
			i.Spec.ForceRegenerate = true
			s.Annotations[crd.AnnotationLastRegenerated] = "1"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copyInstance, copySecret := instance.DeepCopy(), existing.DeepCopy()
			copyTracking := &crd.Tracking{
				DataKeys: append([]string(nil), tracking.DataKeys...), LabelKeys: append([]string(nil), tracking.LabelKeys...),
				Checksums: map[string]string{}, Fingerprints: map[string]string{},
			}
			for key, value := range tracking.Checksums {
				copyTracking.Checksums[key] = value
			}
			for key, value := range tracking.Fingerprints {
				copyTracking.Fingerprints[key] = value
			}
			tt.mutate(copyInstance, copySecret, copyTracking)
			if tt.name == "nil tracking" {
				require.False(t, plan.immutableCurrent(copyInstance, copySecret, nil))
				return
			}
			require.False(t, plan.immutableCurrent(copyInstance, copySecret, copyTracking))
		})
	}

	existing.Annotations[crd.AnnotationLastRegenerated] = "bad"
	require.EqualValues(t, -1, markerOrUnset(existing, instance.Generation))
	require.True(t, sameManagedLabels(map[string]string{"keep": "x"}, map[string]string{"keep": "x"}, []string{"removed"}))
	require.False(t, sameManagedLabels(map[string]string{"keep": "wrong"}, map[string]string{"keep": "x"}, nil))
	require.False(t, sameManagedLabels(map[string]string{"old": "x"}, nil, []string{"old"}))
}

func TestStringGenerationFailureStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "failure", Namespace: "default", Generation: 4}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}

	validationErr := &secret.ValidationError{Field: "test", Err: errors.New("invalid")}
	_, err := r.generationFailure(context.Background(), instance, nil, validationErr)
	require.NoError(t, err)
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(instance), updated))
	require.Equal(t, v1alpha1.ReasonInvalidSpec, updated.Status.Conditions[0].Reason)

	runtimeErr := errors.New("entropy unavailable")
	_, err = r.generationFailure(context.Background(), updated, nil, runtimeErr)
	require.ErrorIs(t, err, runtimeErr)
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(instance), updated))
	require.Equal(t, v1alpha1.ReasonGenerationFailed, updated.Status.Conditions[0].Reason)
}

func TestStringLegacyBaselineIsAdopted(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default", UID: "owner", Generation: 1, Labels: map[string]string{"managed": "yes"}},
		Spec:       v1alpha1.StringSecretSpec{Data: map[string]string{"literal": "value"}, Fields: []v1alpha1.Field{{FieldName: "generated", Length: "6", Encoding: "raw"}}},
	}
	legacy, err := crd.NewSecretWithScheme(instance, map[string][]byte{"literal": []byte("value"), "generated": []byte("123456")}, string(corev1.SecretTypeOpaque), scheme)
	require.NoError(t, err)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, legacy).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)}

	require.NoError(t, reconcileOnce(r, request))
	adopted := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, adopted))
	_, state, trackingErr := crd.LoadTracking(adopted)
	require.NoError(t, trackingErr)
	require.Equal(t, crd.TrackingValid, state)
	require.Equal(t, "yes", adopted.Labels["managed"])
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.True(t, updated.Status.TrackingInitialized)

	adopted.Labels["managed"] = "wrong"
	require.NoError(t, cl.Update(context.Background(), adopted))
	patchErr := errors.New("injected patch failure")
	r.client = &stringPatchFailClient{Client: cl, err: patchErr}
	updatedPlan, err := stringPlanFor(updated)
	require.NoError(t, err)
	_, err = r.update(context.Background(), updated, adopted, updatedPlan, log)
	require.ErrorIs(t, err, patchErr)
}

func TestStringCreateFailuresAreReported(t *testing.T) {
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "create-failure", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Data: map[string]string{"value": "ok"}}}
	plan, err := stringPlanFor(instance)
	require.NoError(t, err)
	_, err = (&ReconcileStringSecret{scheme: runtime.NewScheme()}).create(context.Background(), instance, plan, log)
	require.Error(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	createErr := errors.New("injected create failure")
	r := &ReconcileStringSecret{client: &stringCreateFailClient{Client: base, err: createErr}, scheme: scheme}
	_, err = r.create(context.Background(), instance, plan, log)
	require.ErrorIs(t, err, createErr)
	updated := &v1alpha1.StringSecret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(instance), updated))
	require.Equal(t, v1alpha1.ReasonApplyFailed, updated.Status.Conditions[0].Reason)

	oversized := instance.DeepCopy()
	oversized.Name = "oversized"
	oversized.Spec.Data = map[string]string{"value": string(bytes.Repeat([]byte("x"), secret.MaxProjectedSecretSize))}
	oversizedPlan, err := stringPlanFor(oversized)
	require.NoError(t, err)
	oversizedClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(oversized).WithObjects(oversized).Build()
	_, err = (&ReconcileStringSecret{client: oversizedClient, scheme: scheme}).create(context.Background(), oversized, oversizedPlan, log)
	require.NoError(t, err)
	require.NoError(t, oversizedClient.Get(context.Background(), client.ObjectKeyFromObject(oversized), oversized))
	require.Equal(t, v1alpha1.ReasonSecretSizeConflict, oversized.Status.Conditions[0].Reason)
}

func TestStringMissingFingerprintIsTerminal(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.StringSecret{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "fingerprint", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "6", Encoding: "raw"}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileStringSecret{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)}
	require.NoError(t, reconcileOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	tracking, state, err := crd.LoadTracking(managed)
	require.NoError(t, err)
	require.Equal(t, crd.TrackingValid, state)
	delete(tracking.Fingerprints, "value")
	crd.StoreTracking(managed, tracking)
	require.NoError(t, cl.Update(context.Background(), managed))

	require.NoError(t, reconcileOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, instance))
	require.Equal(t, v1alpha1.ReasonTrackingStateConflict, instance.Status.Conditions[0].Reason)
}

func TestStringReconcileReadErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ReconcileStringSecret{client: empty, scheme: scheme}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"}})
	require.NoError(t, err)

	instance := &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "read-error", Namespace: "default"}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	readErr := errors.New("injected secret read failure")
	r.client = &stringSecretGetFailClient{Client: base, err: readErr}
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	require.ErrorIs(t, err, readErr)
}

func reconcileOnce(r *ReconcileStringSecret, request reconcile.Request) error {
	_, err := r.Reconcile(context.Background(), request)
	return err
}

type failStatusOnceClient struct {
	client.Client
	failures int
}

type stringCreateFailClient struct {
	client.Client
	err error
}

type stringSecretGetFailClient struct {
	client.Client
	err error
}

type stringPatchFailClient struct {
	client.Client
	err error
}

func (c *stringPatchFailClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return c.err
}

func (c *stringSecretGetFailClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*corev1.Secret); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, object, opts...)
}

func (c *stringCreateFailClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return c.err
}

func (c *failStatusOnceClient) Status() client.StatusWriter {
	return &failStatusOnceWriter{SubResourceWriter: c.Client.Status(), client: c}
}

type failStatusOnceWriter struct {
	client.SubResourceWriter
	client *failStatusOnceClient
}

func (w *failStatusOnceWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if w.client.failures > 0 {
		w.client.failures--
		return errors.New("injected status patch failure")
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, opts...)
}
