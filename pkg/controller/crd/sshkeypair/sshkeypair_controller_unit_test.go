package sshkeypair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestSSHManagedKeyBoundaryIncludesEffectiveKeyFields(t *testing.T) {
	literal := make(map[string]string, 254)
	for i := 0; i < 254; i++ {
		literal[fmt.Sprintf("literal-%03d", i)] = "value"
	}
	plan, err := sshPlanFor(&v1alpha1.SSHKeyPair{Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Data: literal}})
	require.NoError(t, err)
	require.Len(t, plan.allKeys, 256)
	literal["literal-overflow"] = "value"
	_, err = sshPlanFor(&v1alpha1.SSHKeyPair{Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Data: literal}})
	require.Error(t, err)
}

func TestPublicKeyDriftIsRepairedWithoutPrivateKeyRotation(t *testing.T) {
	viper.Set("ssh-key-length", 2048)
	viper.Set("ssh-key-algorithm", "rsa")
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.SSHKeyPair{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "ssh", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "rsa", Length: "2048"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileSSHKeyPair{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	before := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, before))
	privateKey := append([]byte(nil), before.Data[secret.DefaultSecretFieldPrivateKey]...)
	before.Data[secret.DefaultSecretFieldPublicKey] = []byte("drift")
	require.NoError(t, cl.Update(context.Background(), before))
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.True(t, bytes.Equal(privateKey, after.Data[secret.DefaultSecretFieldPrivateKey]))
	key, err := secret.ValidateSSHPrivateKey(privateKey, secret.SSHKeyAlgorithmRSA, 2048)
	require.NoError(t, err)
	want, err := secret.SSHPublicKeyForPrivateKey(key)
	require.NoError(t, err)
	require.True(t, bytes.Equal(want, after.Data[secret.DefaultSecretFieldPublicKey]))
}

func TestSSHKeyPairRotationInterval(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.SSHKeyPair{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "scheduled-ssh", Namespace: "default", UID: "owner", Generation: 1},
		Spec:       v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Data: map[string]string{"literal": "stable"}, RotationInterval: "1h"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileSSHKeyPair{client: cl, scheme: scheme, clock: func() time.Time { return now }}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)}
	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, time.Hour, result.RequeueAfter)
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	private := append([]byte(nil), managed.Data[secret.DefaultSecretFieldPrivateKey]...)

	now = now.Add(time.Hour)
	result, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, time.Hour, result.RequeueAfter)
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.NotEqual(t, private, managed.Data[secret.DefaultSecretFieldPrivateKey])
	require.Equal(t, "stable", string(managed.Data["literal"]))
	require.Equal(t, "1", managed.Annotations[crd.AnnotationLastRegenerated])
	_, err = secret.ValidateSSHPrivateKey(managed.Data[secret.DefaultSecretFieldPrivateKey], secret.SSHKeyAlgorithmED25519, 0)
	require.NoError(t, err)
}

func TestSSHRotationIntervalRejectsSuppliedPrivateKey(t *testing.T) {
	_, err := sshPlanFor(&v1alpha1.SSHKeyPair{Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", PrivateKey: "supplied", RotationInterval: "1h"}})
	require.Error(t, err)
}

func TestOwnershipConflictIsTerminalAndDoesNotWriteSecret(t *testing.T) {
	viper.Set("ssh-key-length", 2048)
	viper.Set("ssh-key-algorithm", "rsa")
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	controller := true
	instance := &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "ssh-owner", Namespace: "default", UID: "current-uid", Generation: 3}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "invalid"}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace, Labels: map[string]string{"preserved": "true"}, OwnerReferences: []metav1.OwnerReference{{
		APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind, Name: instance.Name, UID: "old-uid", Controller: &controller,
	}}}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"preserved": []byte("sentinel")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, foreign).Build()
	r := &ReconcileSSHKeyPair{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}

	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Zero(t, result)
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.True(t, reflect.DeepEqual(foreign, after))
	updated := &v1alpha1.SSHKeyPair{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, int64(3), updated.Status.ObservedGeneration)
	require.Equal(t, v1alpha1.ReasonSecretOwnershipConflict, updated.Status.Conditions[0].Reason)
	statusVersion := updated.ResourceVersion
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, statusVersion, updated.ResourceVersion)
}

func TestSecretTypeMismatchIsTerminalAndDoesNotWrite(t *testing.T) {
	viper.Set("ssh-key-length", 2048)
	viper.Set("ssh-key-algorithm", "rsa")
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.SSHKeyPair{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "type-change", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Type: "example.test/one"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileSSHKeyPair{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	before := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, before))
	before.Type = "example.test/out-of-band"
	require.NoError(t, cl.Update(context.Background(), before))
	secretVersion := before.ResourceVersion
	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Zero(t, result)
	after := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.Equal(t, secretVersion, after.ResourceVersion)
	require.Equal(t, corev1.SecretType("example.test/out-of-band"), after.Type)

	updated := &v1alpha1.SSHKeyPair{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, v1alpha1.ReasonSecretTypeConflict, updated.Status.Conditions[0].Reason)
	statusVersion := updated.ResourceVersion
	_, err = r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, after))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, updated))
	require.Equal(t, secretVersion, after.ResourceVersion)
	require.Equal(t, statusVersion, updated.ResourceVersion)
}

func TestSSHPlanValidationAndLegacyBaseline(t *testing.T) {
	viper.Set("ssh-key-length", 2048)
	viper.Set("ssh-key-algorithm", "rsa")
	instance := &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Data: map[string]string{"literal": "value"}}}
	plan, err := sshPlanFor(instance)
	require.NoError(t, err)
	data := map[string][]byte{"literal": []byte("value")}
	require.NoError(t, secret.GenerateSSHKeypairDataWithAlgorithm(log, plan.algorithm, plan.length, plan.privateField, plan.publicField, true, data))
	existing := &corev1.Secret{Type: corev1.SecretTypeOpaque, Data: data}
	require.NoError(t, plan.validateLegacy(instance, existing))

	for _, tt := range []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{name: "type", mutate: func(s *corev1.Secret) { s.Type = corev1.SecretTypeTLS }},
		{name: "literal", mutate: func(s *corev1.Secret) { s.Data["literal"] = []byte("drift") }},
		{name: "private key", mutate: func(s *corev1.Secret) { s.Data[plan.privateField] = []byte("bad") }},
		{name: "public key", mutate: func(s *corev1.Secret) { s.Data[plan.publicField] = []byte("bad") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copySecret := existing.DeepCopy()
			tt.mutate(copySecret)
			require.Error(t, plan.validateLegacy(instance, copySecret))
		})
	}

	suppliedInstance := instance.DeepCopy()
	suppliedInstance.Spec.PrivateKey = string(data[plan.privateField])
	suppliedPlan, err := sshPlanFor(suppliedInstance)
	require.NoError(t, err)
	require.NoError(t, suppliedPlan.validateLegacy(suppliedInstance, existing))
	mismatch := existing.DeepCopy()
	mismatch.Data[plan.privateField] = []byte("different")
	require.Error(t, suppliedPlan.validateLegacy(suppliedInstance, mismatch))

	for _, spec := range []v1alpha1.SSHKeyPairSpec{
		{Algorithm: "ed25519", PrivateKeyField: "same", PublicKeyField: "same"},
		{Algorithm: "ed25519", Data: map[string]string{secret.DefaultSecretFieldPrivateKey: "collision"}},
		{Algorithm: "invalid"},
		{Algorithm: "ed25519", PrivateKey: "bad"},
	} {
		_, err = sshPlanFor(&v1alpha1.SSHKeyPair{Spec: spec})
		require.Error(t, err)
	}
	rsaPlan, err := sshPlanFor(&v1alpha1.SSHKeyPair{Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "rsa"}})
	require.NoError(t, err)
	require.Equal(t, "2048", rsaPlan.length)
}

func TestSSHImmutableAndMarkerHelpers(t *testing.T) {
	instance := &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}, Generation: 2}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Data: map[string]string{"literal": "value"}}}
	plan, err := sshPlanFor(instance)
	require.NoError(t, err)
	data := map[string][]byte{"literal": []byte("value")}
	require.NoError(t, secret.GenerateSSHKeypairDataWithAlgorithm(log, plan.algorithm, plan.length, plan.privateField, plan.publicField, true, data))
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"managed": "yes"}, Annotations: map[string]string{crd.AnnotationLastRegenerated: "2"}}, Type: corev1.SecretTypeOpaque, Data: data}
	tracking := plan.tracking(instance)
	crd.RefreshChecksums(tracking, existing.Data)
	require.True(t, plan.immutableCurrent(instance, existing, tracking))
	require.EqualValues(t, 2, markerOrUnset(existing, instance.Generation))

	for _, tt := range []struct {
		name   string
		mutate func(*v1alpha1.SSHKeyPair, *corev1.Secret, *crd.Tracking)
	}{
		{name: "labels", mutate: func(_ *v1alpha1.SSHKeyPair, s *corev1.Secret, _ *crd.Tracking) { s.Labels["managed"] = "no" }},
		{name: "removed label remains", mutate: func(i *v1alpha1.SSHKeyPair, _ *corev1.Secret, _ *crd.Tracking) { delete(i.Labels, "managed") }},
		{name: "literal", mutate: func(_ *v1alpha1.SSHKeyPair, s *corev1.Secret, _ *crd.Tracking) { s.Data["literal"] = []byte("drift") }},
		{name: "fingerprint", mutate: func(_ *v1alpha1.SSHKeyPair, _ *corev1.Secret, tr *crd.Tracking) {
			tr.Fingerprints[keyFingerprint] = "bad"
		}},
		{name: "checksum", mutate: func(_ *v1alpha1.SSHKeyPair, _ *corev1.Secret, tr *crd.Tracking) {
			tr.Checksums[plan.privateField] = "bad"
		}},
		{name: "force", mutate: func(i *v1alpha1.SSHKeyPair, s *corev1.Secret, _ *crd.Tracking) {
			i.Spec.ForceRegenerate = true
			s.Annotations[crd.AnnotationLastRegenerated] = "1"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copyInstance, copySecret := instance.DeepCopy(), existing.DeepCopy()
			copyTracking := cloneSSHTracking(tracking)
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

func TestSSHGenerationFailureStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "failure", Namespace: "default", Generation: 4}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	r := &ReconcileSSHKeyPair{client: cl, scheme: scheme}
	_, err := r.generationFailure(context.Background(), instance, nil, &secret.ValidationError{Field: "test", Err: errors.New("invalid")})
	require.NoError(t, err)
	updated := &v1alpha1.SSHKeyPair{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(instance), updated))
	require.Equal(t, v1alpha1.ReasonInvalidSpec, updated.Status.Conditions[0].Reason)
	runtimeErr := errors.New("entropy unavailable")
	_, err = r.generationFailure(context.Background(), updated, nil, runtimeErr)
	require.ErrorIs(t, err, runtimeErr)
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(instance), updated))
	require.Equal(t, v1alpha1.ReasonGenerationFailed, updated.Status.Conditions[0].Reason)
}

func TestSSHLegacyBaselineForceAndImmutable(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	instance := &v1alpha1.SSHKeyPair{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default", UID: "owner", Generation: 1, Labels: map[string]string{"managed": "yes"}}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519"}}
	plan, err := sshPlanFor(instance)
	require.NoError(t, err)
	data := map[string][]byte{}
	require.NoError(t, secret.GenerateSSHKeypairDataWithAlgorithm(log, plan.algorithm, plan.length, plan.privateField, plan.publicField, true, data))
	legacy, err := crd.NewSecretWithScheme(instance, data, string(corev1.SecretTypeOpaque), scheme)
	require.NoError(t, err)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, legacy).Build()
	r := &ReconcileSSHKeyPair{client: cl, scheme: scheme}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)}
	require.NoError(t, reconcileSSHOnce(r, request))
	managed := &corev1.Secret{}
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	_, state, trackingErr := crd.LoadTracking(managed)
	require.NoError(t, trackingErr)
	require.Equal(t, crd.TrackingValid, state)

	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, instance))
	instance.Spec.ForceRegenerate = true
	instance.Generation = 2
	require.NoError(t, cl.Update(context.Background(), instance))
	require.NoError(t, reconcileSSHOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, managed))
	require.Equal(t, "2", managed.Annotations[crd.AnnotationLastRegenerated])
	immutable := true
	managed.Immutable = &immutable
	require.NoError(t, cl.Update(context.Background(), managed))
	require.NoError(t, reconcileSSHOnce(r, request))
	require.NoError(t, cl.Get(context.Background(), request.NamespacedName, instance))
	require.Equal(t, v1alpha1.ReasonReconciled, instance.Status.Conditions[0].Reason)
}

func TestSSHCreateFailuresAndReadErrors(t *testing.T) {
	instance := &v1alpha1.SSHKeyPair{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind}, ObjectMeta: metav1.ObjectMeta{Name: "create-failure", Namespace: "default", UID: "owner", Generation: 1}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519"}}
	plan, err := sshPlanFor(instance)
	require.NoError(t, err)
	_, err = (&ReconcileSSHKeyPair{scheme: runtime.NewScheme()}).create(context.Background(), instance, plan, log)
	require.Error(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	createErr := errors.New("injected create failure")
	r := &ReconcileSSHKeyPair{client: &sshCreateFailClient{Client: base, err: createErr}, scheme: scheme}
	_, err = r.create(context.Background(), instance, plan, log)
	require.ErrorIs(t, err, createErr)

	oversized := instance.DeepCopy()
	oversized.Name = "oversized"
	oversized.Spec.Data = map[string]string{"value": strings.Repeat("x", secret.MaxProjectedSecretSize)}
	oversizedPlan, err := sshPlanFor(oversized)
	require.NoError(t, err)
	oversizedClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(oversized).WithObjects(oversized).Build()
	_, err = (&ReconcileSSHKeyPair{client: oversizedClient, scheme: scheme}).create(context.Background(), oversized, oversizedPlan, log)
	require.NoError(t, err)
	require.NoError(t, oversizedClient.Get(context.Background(), client.ObjectKeyFromObject(oversized), oversized))
	require.Equal(t, v1alpha1.ReasonSecretSizeConflict, oversized.Status.Conditions[0].Reason)

	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err = (&ReconcileSSHKeyPair{client: empty, scheme: scheme}).Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"}})
	require.NoError(t, err)
	readErr := errors.New("injected secret read failure")
	r.client = &sshSecretGetFailClient{Client: base, err: readErr}
	_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	require.ErrorIs(t, err, readErr)

	invalid := instance.DeepCopy()
	invalid.Name = "invalid"
	invalid.Spec.Algorithm = "bad"
	invalidClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(invalid).WithObjects(invalid).Build()
	_, err = (&ReconcileSSHKeyPair{client: invalidClient, scheme: scheme}).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(invalid)})
	require.NoError(t, err)
	require.NoError(t, invalidClient.Get(context.Background(), client.ObjectKeyFromObject(invalid), invalid))
	require.Equal(t, v1alpha1.ReasonInvalidSpec, invalid.Status.Conditions[0].Reason)
}

func TestSSHUpdateConflictBranches(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.SchemeBuilder.AddToScheme(scheme))
	baseInstance := &v1alpha1.SSHKeyPair{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "update", Namespace: "default", UID: "owner", Generation: 2, Labels: map[string]string{"managed": "yes"}},
		Spec:       v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519"},
		Status:     v1alpha1.SSHKeyPairStatus{CommonSecretStatus: v1alpha1.CommonSecretStatus{TrackingInitialized: true}},
	}
	plan, err := sshPlanFor(baseInstance)
	require.NoError(t, err)
	data := map[string][]byte{}
	require.NoError(t, secret.GenerateSSHKeypairDataWithAlgorithm(log, plan.algorithm, plan.length, plan.privateField, plan.publicField, true, data))
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
			delete(tr.Fingerprints, keyFingerprint)
			crd.StoreTracking(s, tr)
		}, wantReason: v1alpha1.ReasonTrackingStateConflict},
		{name: "tracking absent after initialization", initialized: true, mutate: removeSSHTracking, wantReason: v1alpha1.ReasonTrackingStateConflict},
		{name: "invalid legacy baseline", mutate: func(s *corev1.Secret) { removeSSHTracking(s); s.Data[plan.publicField] = []byte("bad") }, wantReason: v1alpha1.ReasonLegacyBaselineInvalid},
		{name: "immutable drift", initialized: true, mutate: func(s *corev1.Secret) {
			immutable := true
			s.Immutable = &immutable
			s.Data[plan.publicField] = []byte("bad")
		}, wantReason: v1alpha1.ReasonImmutableSecretConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			instance, existing := baseInstance.DeepCopy(), baseSecret.DeepCopy()
			instance.Status.TrackingInitialized = tt.initialized
			tt.mutate(existing)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, existing).Build()
			r := &ReconcileSSHKeyPair{client: cl, scheme: scheme}
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
	r := &ReconcileSSHKeyPair{client: &sshPatchFailClient{Client: base, err: patchErr}, scheme: scheme}
	_, err = r.update(context.Background(), instance, existing, plan, log)
	require.ErrorIs(t, err, patchErr)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(instance), instance))
	require.Equal(t, v1alpha1.ReasonApplyFailed, instance.Status.Conditions[0].Reason)

	supplied := baseInstance.DeepCopy()
	supplied.Name = "supplied"
	supplied.Spec.PrivateKey = string(baseSecret.Data[plan.privateField])
	supplied.Spec.ForceRegenerate = true
	suppliedPlan, err := sshPlanFor(supplied)
	require.NoError(t, err)
	suppliedSecret, err := crd.NewSecretWithScheme(supplied, map[string][]byte{plan.privateField: append([]byte(nil), baseSecret.Data[plan.privateField]...), plan.publicField: append([]byte(nil), baseSecret.Data[plan.publicField]...)}, string(corev1.SecretTypeOpaque), scheme)
	require.NoError(t, err)
	crd.SetRegenerationMarker(suppliedSecret, 1)
	suppliedTracking := suppliedPlan.tracking(supplied)
	crd.RefreshChecksums(suppliedTracking, suppliedSecret.Data)
	crd.StoreTracking(suppliedSecret, suppliedTracking)
	suppliedClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(supplied).WithObjects(supplied, suppliedSecret).Build()
	_, err = (&ReconcileSSHKeyPair{client: suppliedClient, scheme: scheme}).update(context.Background(), supplied, suppliedSecret, suppliedPlan, log)
	require.NoError(t, err)
	require.NoError(t, suppliedClient.Get(context.Background(), client.ObjectKeyFromObject(suppliedSecret), suppliedSecret))
	require.True(t, bytes.Equal([]byte(supplied.Spec.PrivateKey), suppliedSecret.Data[plan.privateField]))
}

func reconcileSSHOnce(r *ReconcileSSHKeyPair, request reconcile.Request) error {
	_, err := r.Reconcile(context.Background(), request)
	return err
}

func cloneSSHTracking(in *crd.Tracking) *crd.Tracking {
	out := &crd.Tracking{DataKeys: append([]string(nil), in.DataKeys...), LabelKeys: append([]string(nil), in.LabelKeys...), Checksums: map[string]string{}, Fingerprints: map[string]string{}}
	for key, value := range in.Checksums {
		out.Checksums[key] = value
	}
	for key, value := range in.Fingerprints {
		out.Fingerprints[key] = value
	}
	return out
}

func removeSSHTracking(s *corev1.Secret) {
	for _, key := range []string{crd.AnnotationTrackingVersion, crd.AnnotationManagedDataKeys, crd.AnnotationManagedLabelKeys, crd.AnnotationManagedDataChecksums, crd.AnnotationGenerationFingerprint, crd.AnnotationManagedKeysDigest} {
		delete(s.Annotations, key)
	}
}

type sshCreateFailClient struct {
	client.Client
	err error
}

func (c *sshCreateFailClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return c.err
}

type sshSecretGetFailClient struct {
	client.Client
	err error
}

func (c *sshSecretGetFailClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*corev1.Secret); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, object, opts...)
}

type sshPatchFailClient struct {
	client.Client
	err error
}

func (c *sshPatchFailClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return c.err
}
