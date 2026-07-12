package basicauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
)

func TestBasicAuthManagedKeyBoundaryIncludesGeneratedFields(t *testing.T) {
	literal := make(map[string]string, 253)
	for i := 0; i < 253; i++ {
		literal[fmt.Sprintf("literal-%03d", i)] = "value"
	}
	plan, err := basicPlanFor(&v1alpha1.BasicAuth{Spec: v1alpha1.BasicAuthSpec{Data: literal, Length: "24", Encoding: "base64"}})
	require.NoError(t, err)
	require.Len(t, plan.allKeys, 256)
	literal["literal-overflow"] = "value"
	_, err = basicPlanFor(&v1alpha1.BasicAuth{Spec: v1alpha1.BasicAuthSpec{Data: literal, Length: "24", Encoding: "base64"}})
	require.Error(t, err)
}

func TestBasicAuthDriftRotatesCredentialSet(t *testing.T) {
	viper.Set("secret-length", 24)
	viper.Set("secret-encoding", "base64")
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.BasicAuth{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.BasicAuthSpec{Username: "user", Length: "24", Encoding: "base64"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileBasicAuth{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	before := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, before))
	oldPassword := append([]byte(nil), before.Data[secret.FieldBasicAuthPassword]...)
	before.Data[secret.FieldBasicAuthUsername] = []byte("drift")
	require.NoError(t, cl.Update(context.Background(), before))
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.False(t, bytes.Equal(oldPassword, after.Data[secret.FieldBasicAuthPassword]))
	require.True(t, string(after.Data[secret.FieldBasicAuthUsername]) == "user")
	require.NoError(t, secret.ValidateBasicAuthCredential(after.Data))
}

func TestBasicAuthSecretTypeMismatchIsTerminal(t *testing.T) {
	viper.Set("secret-length", 24)
	viper.Set("secret-encoding", "base64")
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.BasicAuth{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "auth-type", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.BasicAuthSpec{Length: "24", Encoding: "base64"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileBasicAuth{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	managed.Type = corev1.SecretTypeTLS
	require.NoError(t, cl.Update(context.Background(), managed))
	version := managed.ResourceVersion
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.Equal(t, version, managed.ResourceVersion)
	updated := &v1alpha1.BasicAuth{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, v1alpha1.ReasonSecretTypeConflict, updated.Status.Conditions[0].Reason)
}

func TestOwnershipConflictIsTerminalAndDoesNotWriteSecret(t *testing.T) {
	viper.Set("secret-length", 24)
	viper.Set("secret-encoding", "base64")
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	controller := true
	instance := &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "auth-owner", Namespace: "default", UID: "current-uid", Generation: 3}, Spec: v1alpha1.BasicAuthSpec{Length: "73", Encoding: "raw"}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace, Labels: map[string]string{"preserved": "true"}, OwnerReferences: []metav1.OwnerReference{{
		APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind, Name: instance.Name, UID: "old-uid", Controller: &controller,
	}}}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"preserved": []byte("sentinel")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, foreign).Build()
	r := &ReconcileBasicAuth{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}

	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Zero(t, result)
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.True(t, reflect.DeepEqual(foreign, after))
	updated := &v1alpha1.BasicAuth{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, int64(3), updated.Status.ObservedGeneration)
	require.Equal(t, v1alpha1.ReasonSecretOwnershipConflict, updated.Status.Conditions[0].Reason)
	statusVersion := updated.ResourceVersion
	require.NoError(t, reconcileOwnershipConflict(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, statusVersion, updated.ResourceVersion)
}

func reconcileOwnershipConflict(r *ReconcileBasicAuth, request reconcile.Request) error {
	_, err := r.Reconcile(context.Background(), request)
	return err
}

func TestBasicPlanValidationAndLegacyBaseline(t *testing.T) {
	viper.Set("secret-length", 24)
	viper.Set("secret-encoding", "base64")
	defaultPlan, err := basicPlanFor(&v1alpha1.BasicAuth{})
	require.NoError(t, err)
	require.Equal(t, "admin", defaultPlan.constraints.Username)
	require.Equal(t, secret.DefaultEncoding(), defaultPlan.constraints.Encoding)

	instance := &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}}, Spec: v1alpha1.BasicAuthSpec{Data: map[string]string{"literal": "value"}, Username: "user", Length: "6", Encoding: "raw"}}
	plan, err := basicPlanFor(instance)
	require.NoError(t, err)
	data := map[string][]byte{"literal": []byte("value")}
	require.NoError(t, secret.GenerateBasicAuthData(log, &plan.constraints, data))
	existing := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: data}
	require.NoError(t, plan.validateLegacy(instance, existing))

	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{name: "type", mutate: func(s *corev1.Secret) { s.Type = corev1.SecretTypeTLS }},
		{name: "literal", mutate: func(s *corev1.Secret) { s.Data["literal"] = []byte("drift") }},
		{name: "username", mutate: func(s *corev1.Secret) { s.Data[secret.FieldBasicAuthUsername] = []byte("other") }},
		{name: "password length", mutate: func(s *corev1.Secret) { s.Data[secret.FieldBasicAuthPassword] = []byte("short") }},
		{name: "credential", mutate: func(s *corev1.Secret) { s.Data[secret.FieldBasicAuthIngress] = []byte("bad") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copySecret := existing.DeepCopy()
			tt.mutate(copySecret)
			require.Error(t, plan.validateLegacy(instance, copySecret))
		})
	}
	invalidLegacyPlan := *plan
	invalidLegacyPlan.constraints.Length = "bad"
	require.Error(t, invalidLegacyPlan.validateLegacy(instance, existing))

	for _, spec := range []v1alpha1.BasicAuthSpec{
		{Data: map[string]string{secret.FieldBasicAuthPassword: "reserved"}, Length: "6", Encoding: "raw"},
		{Username: "bad:name", Length: "6", Encoding: "raw"},
		{Length: "bad", Encoding: "raw"},
		{Length: "73", Encoding: "raw"},
	} {
		_, err = basicPlanFor(&v1alpha1.BasicAuth{Spec: spec})
		require.Error(t, err)
	}
}

func TestBasicImmutableAndMarkerHelpers(t *testing.T) {
	instance := &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}, Generation: 2}, Spec: v1alpha1.BasicAuthSpec{Data: map[string]string{"literal": "value"}, Username: "user", Length: "6", Encoding: "raw"}}
	plan, err := basicPlanFor(instance)
	require.NoError(t, err)
	data := map[string][]byte{"literal": []byte("value")}
	require.NoError(t, secret.GenerateBasicAuthData(log, &plan.constraints, data))
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}, Annotations: map[string]string{crd.AnnotationLastRegenerated: "2"}}, Type: corev1.SecretTypeOpaque, Data: data}
	tracking := plan.tracking(instance)
	crd.RefreshChecksums(tracking, existing.Data)
	require.True(t, plan.immutableCurrent(instance, existing, tracking))
	require.EqualValues(t, 2, markerOrUnset(existing, instance.Generation))

	for _, tt := range []struct {
		name   string
		mutate func(*v1alpha1.BasicAuth, *corev1.Secret, *crd.Tracking)
	}{
		{name: "labels", mutate: func(_ *v1alpha1.BasicAuth, s *corev1.Secret, _ *crd.Tracking) { s.Labels["managed"] = "no" }},
		{name: "removed label remains", mutate: func(i *v1alpha1.BasicAuth, _ *corev1.Secret, _ *crd.Tracking) { delete(i.Labels, "managed") }},
		{name: "literal", mutate: func(_ *v1alpha1.BasicAuth, s *corev1.Secret, _ *crd.Tracking) { s.Data["literal"] = []byte("drift") }},
		{name: "fingerprint", mutate: func(_ *v1alpha1.BasicAuth, _ *corev1.Secret, tr *crd.Tracking) {
			tr.Fingerprints[credentialFingerprint] = "bad"
		}},
		{name: "checksum", mutate: func(_ *v1alpha1.BasicAuth, _ *corev1.Secret, tr *crd.Tracking) {
			tr.Checksums[secret.FieldBasicAuthPassword] = "bad"
		}},
		{name: "force", mutate: func(i *v1alpha1.BasicAuth, s *corev1.Secret, _ *crd.Tracking) {
			i.Spec.ForceRegenerate = true
			s.Annotations[crd.AnnotationLastRegenerated] = "1"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copyInstance, copySecret := instance.DeepCopy(), existing.DeepCopy()
			copyTracking := cloneBasicTracking(tracking)
			tt.mutate(copyInstance, copySecret, copyTracking)
			require.False(t, plan.immutableCurrent(copyInstance, copySecret, copyTracking))
		})
	}
	require.False(t, plan.immutableCurrent(instance, existing, nil))
	existing.Annotations[crd.AnnotationLastRegenerated] = "bad"
	require.EqualValues(t, -1, markerOrUnset(existing, instance.Generation))
	require.True(t, sameManagedLabels(map[string]string{"keep": "x"}, map[string]string{"keep": "x"}, []string{"removed"}))
	require.False(t, sameManagedLabels(map[string]string{"keep": "wrong"}, map[string]string{"keep": "x"}, nil))
	require.False(t, sameManagedLabels(map[string]string{"old": "x"}, nil, []string{"old"}))
}

func TestBasicGenerationFailureStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "failure", Namespace: "default", Generation: 4}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileBasicAuth{client: cl, scheme: scheme}
	_, err := r.generationFailure(context.Background(), instance, nil, &secret.ValidationError{Field: "test", Err: errors.New("invalid")})
	require.NoError(t, err)
	updated := &v1alpha1.BasicAuth{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(instance), updated))
	require.Equal(t, v1alpha1.ReasonInvalidSpec, updated.Status.Conditions[0].Reason)
	runtimeErr := errors.New("entropy unavailable")
	_, err = r.generationFailure(context.Background(), updated, nil, runtimeErr)
	require.ErrorIs(t, err, runtimeErr)
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(instance), updated))
	require.Equal(t, v1alpha1.ReasonGenerationFailed, updated.Status.Conditions[0].Reason)
}

func TestBasicLegacyBaselineForceAndImmutable(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.BasicAuth{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default", UID: "owner", Generation: 1, Labels: map[string]string{"managed": "yes"}}, Spec: v1alpha1.BasicAuthSpec{Username: "user", Length: "6", Encoding: "raw"}}
	plan, err := basicPlanFor(instance)
	require.NoError(t, err)
	data := map[string][]byte{}
	require.NoError(t, secret.GenerateBasicAuthData(log, &plan.constraints, data))
	legacy, err := crd.NewSecretWithScheme(instance, data, string(corev1.SecretTypeOpaque), scheme)
	require.NoError(t, err)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, legacy).Build()
	r := &ReconcileBasicAuth{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)}
	require.NoError(t, reconcileBasicOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	_, state, trackingErr := crd.LoadTracking(managed)
	require.NoError(t, trackingErr)
	require.Equal(t, crd.TrackingValid, state)

	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, instance))
	instance.Spec.ForceRegenerate = true
	instance.Generation = 2
	require.NoError(t, cl.Update(context.Background(), instance))
	require.NoError(t, reconcileBasicOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.Equal(t, "2", managed.Annotations[crd.AnnotationLastRegenerated])
	immutable := true
	managed.Immutable = &immutable
	require.NoError(t, cl.Update(context.Background(), managed))
	require.NoError(t, reconcileBasicOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, instance))
	require.Equal(t, v1alpha1.ReasonReconciled, instance.Status.Conditions[0].Reason)
}

func TestBasicCreateFailuresAndReadErrors(t *testing.T) {
	instance := &v1alpha1.BasicAuth{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "create-failure", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.BasicAuthSpec{Length: "6", Encoding: "raw"}}
	plan, err := basicPlanFor(instance)
	require.NoError(t, err)
	_, err = (&ReconcileBasicAuth{scheme: runtime.NewScheme()}).create(context.Background(), instance, plan, log)
	require.Error(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	createErr := errors.New("injected create failure")
	r := &ReconcileBasicAuth{client: &basicCreateFailClient{Client: base, err: createErr}, scheme: scheme}
	_, err = r.create(context.Background(), instance, plan, log)
	require.ErrorIs(t, err, createErr)

	oversized := instance.DeepCopy()
	oversized.Name = "oversized"
	oversized.Spec.Data = map[string]string{"value": strings.Repeat("x", secret.MaxProjectedSecretSize)}
	oversizedPlan, err := basicPlanFor(oversized)
	require.NoError(t, err)
	oversizedClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(oversized).WithObjects(oversized).Build()
	_, err = (&ReconcileBasicAuth{client: oversizedClient, scheme: scheme}).create(context.Background(), oversized, oversizedPlan, log)
	require.NoError(t, err)
	require.NoError(t, oversizedClient.Get(context.Background(), client.ObjectKeyFromObject(oversized), oversized))
	require.Equal(t, v1alpha1.ReasonSecretSizeConflict, oversized.Status.Conditions[0].Reason)

	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err = (&ReconcileBasicAuth{client: empty, scheme: scheme}).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"}})
	require.NoError(t, err)
	readErr := errors.New("injected secret read failure")
	r.client = &basicSecretGetFailClient{Client: base, err: readErr}
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	require.ErrorIs(t, err, readErr)

	invalid := instance.DeepCopy()
	invalid.Name = "invalid"
	invalid.Spec.Length = "bad"
	invalidClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(invalid).WithObjects(invalid).Build()
	_, err = (&ReconcileBasicAuth{client: invalidClient, scheme: scheme}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(invalid)})
	require.NoError(t, err)
	require.NoError(t, invalidClient.Get(context.Background(), client.ObjectKeyFromObject(invalid), invalid))
	require.Equal(t, v1alpha1.ReasonInvalidSpec, invalid.Status.Conditions[0].Reason)
}

func TestBasicUpdateConflictBranches(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	baseInstance := &v1alpha1.BasicAuth{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "update", Namespace: "default", UID: "owner", Generation: 2, Labels: map[string]string{"managed": "yes"}},
		Spec:       v1alpha1.BasicAuthSpec{Username: "user", Length: "6", Encoding: "raw"},
		Status:     v1alpha1.BasicAuthStatus{CommonSecretStatus: v1alpha1.CommonSecretStatus{TrackingInitialized: true}},
	}
	plan, err := basicPlanFor(baseInstance)
	require.NoError(t, err)
	data := map[string][]byte{}
	require.NoError(t, secret.GenerateBasicAuthData(log, &plan.constraints, data))
	baseSecret, err := crd.NewSecretWithScheme(baseInstance, data, string(corev1.SecretTypeOpaque), scheme)
	require.NoError(t, err)
	crd.SetRegenerationMarker(baseSecret, baseInstance.Generation)
	tracking := plan.tracking(baseInstance)
	crd.RefreshChecksums(tracking, baseSecret.Data)
	crd.StoreTracking(baseSecret, tracking)

	for _, tt := range []struct {
		name        string
		initialized bool
		mutate      func(*corev1.Secret)
		wantReason  string
	}{
		{name: "malformed marker", initialized: true, mutate: func(s *corev1.Secret) { s.Annotations[crd.AnnotationLastRegenerated] = "bad" }, wantReason: v1alpha1.ReasonRegenerationStateConflict},
		{name: "missing fingerprint", initialized: true, mutate: func(s *corev1.Secret) {
			tr, _, _ := crd.LoadTracking(s)
			delete(tr.Fingerprints, credentialFingerprint)
			crd.StoreTracking(s, tr)
		}, wantReason: v1alpha1.ReasonTrackingStateConflict},
		{name: "tracking absent after initialization", initialized: true, mutate: removeBasicTracking, wantReason: v1alpha1.ReasonTrackingStateConflict},
		{name: "invalid legacy baseline", mutate: func(s *corev1.Secret) {
			removeBasicTracking(s)
			s.Data[secret.FieldBasicAuthUsername] = []byte("other")
		}, wantReason: v1alpha1.ReasonLegacyBaselineInvalid},
		{name: "immutable drift", initialized: true, mutate: func(s *corev1.Secret) {
			immutable := true
			s.Immutable = &immutable
			s.Data[secret.FieldBasicAuthUsername] = []byte("other")
		}, wantReason: v1alpha1.ReasonImmutableSecretConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			instance, existing := baseInstance.DeepCopy(), baseSecret.DeepCopy()
			instance.Status.TrackingInitialized = tt.initialized
			tt.mutate(existing)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, existing).Build()
			r := &ReconcileBasicAuth{client: cl, scheme: scheme}
			_, err := r.update(context.Background(), instance, existing, plan, log)
			require.NoError(t, err)
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(instance), instance))
			require.Equal(t, tt.wantReason, instance.Status.Conditions[0].Reason)
		})
	}

	instance, existing := baseInstance.DeepCopy(), baseSecret.DeepCopy()
	existing.Labels["managed"] = "wrong"
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, existing).Build()
	patchErr := errors.New("injected patch failure")
	r := &ReconcileBasicAuth{client: &basicPatchFailClient{Client: base, err: patchErr}, scheme: scheme}
	_, err = r.update(context.Background(), instance, existing, plan, log)
	require.ErrorIs(t, err, patchErr)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(instance), instance))
	require.Equal(t, v1alpha1.ReasonApplyFailed, instance.Status.Conditions[0].Reason)
}

func reconcileBasicOnce(r *ReconcileBasicAuth, request reconcile.Request) error {
	_, err := r.Reconcile(context.Background(), request)
	return err
}

func cloneBasicTracking(in *crd.Tracking) *crd.Tracking {
	out := &crd.Tracking{DataKeys: append([]string(nil), in.DataKeys...), LabelKeys: append([]string(nil), in.LabelKeys...), Checksums: map[string]string{}, Fingerprints: map[string]string{}}
	for key, value := range in.Checksums {
		out.Checksums[key] = value
	}
	for key, value := range in.Fingerprints {
		out.Fingerprints[key] = value
	}
	return out
}

func removeBasicTracking(s *corev1.Secret) {
	for _, key := range []string{crd.AnnotationTrackingVersion, crd.AnnotationManagedDataKeys, crd.AnnotationManagedLabelKeys, crd.AnnotationManagedDataChecksums, crd.AnnotationGenerationFingerprint, crd.AnnotationManagedKeysDigest} {
		delete(s.Annotations, key)
	}
}

type basicCreateFailClient struct {
	client.Client
	err error
}

func (c *basicCreateFailClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return c.err
}

type basicSecretGetFailClient struct {
	client.Client
	err error
}

func (c *basicSecretGetFailClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*corev1.Secret); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, object, opts...)
}

type basicPatchFailClient struct {
	client.Client
	err error
}

func (c *basicPatchFailClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return c.err
}
