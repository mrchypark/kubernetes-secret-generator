package sshkeypair

import (
	"bytes"
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
)

func TestDueRotationAndSuppliedPrivateKeyGuard(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 2, 3, 4, time.UTC)
	t.Run("due rotation preserves unrelated fields", func(t *testing.T) {
		controller := true
		cr := &v1alpha1.SSHKeyPair{
			TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
			ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "owner-uid"},
			Spec: v1alpha1.SSHKeyPairSpec{
				Algorithm: secret.SSHKeyAlgorithmED25519, Data: map[string]string{"literal": "literal"}, RotationInterval: "1h",
			},
		}
		existing := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "example", Namespace: "default", Labels: map[string]string{"preserve": "yes"},
				Annotations:     map[string]string{crd.RotationAnchorAnnotation: now.Add(-2 * time.Hour).Format(time.RFC3339Nano)},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind, Name: "example", UID: "owner-uid", Controller: &controller}},
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{secret.SecretFieldPrivateKey: []byte("old-private"), secret.SecretFieldPublicKey: []byte("old-public"), "literal": []byte("literal"), "unmanaged": []byte("keep")},
		}

		c, scheme := sshClient(t, cr, existing)
		r := &ReconcileSSHKeyPair{client: c, scheme: scheme, now: func() time.Time { return now }}
		result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter != time.Hour {
			t.Fatalf("RequeueAfter = %s, want 1h", result.RequeueAfter)
		}
		got := &corev1.Secret{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), got); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got.Data[secret.SecretFieldPrivateKey], existing.Data[secret.SecretFieldPrivateKey]) {
			t.Fatal("private key was not rotated")
		}
		if string(got.Data["literal"]) != "literal" || string(got.Data["unmanaged"]) != "keep" || got.Type != existing.Type || got.Labels["preserve"] != "yes" {
			t.Fatal("literal data, unmanaged data, type, or labels changed")
		}
	})

	t.Run("supplied private key rejects interval without mutation", func(t *testing.T) {
		r := &ReconcileSSHKeyPair{now: func() time.Time { return now }}
		target := &corev1.Secret{Data: map[string][]byte{"keep": []byte("yes")}}
		before := target.DeepCopy()
		_, _, err := r.scheduleRotation(&v1alpha1.SSHKeyPair{Spec: v1alpha1.SSHKeyPairSpec{PrivateKey: "supplied", RotationInterval: "1h"}}, target)
		if err == nil {
			t.Fatal("scheduleRotation() error = nil, want error")
		}
		if !bytes.Equal(target.Data["keep"], before.Data["keep"]) || len(target.Annotations) != 0 {
			t.Fatal("Secret mutated on invalid supplied-key rotation")
		}
	})
}

func sshClient(t *testing.T, objects ...client.Object) (client.Client, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.SSHKeyPair{}).WithObjects(objects...).Build(), scheme
}
