package sshkeypair

import (
	"bytes"
	"context"
	"reflect"
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

func TestPartialSSHKeyRepair(t *testing.T) {
	valid := make(map[string][]byte)
	if err := secret.GenerateSSHKeypairDataWithAlgorithm(log, secret.SSHKeyAlgorithmED25519, "", secret.SecretFieldPrivateKey, secret.SecretFieldPublicKey, true, valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		privateValue []byte
		publicValue  []byte
		wantErr      bool
	}{
		{"private missing", nil, bytes.Clone(valid[secret.SecretFieldPublicKey]), true},
		{"private empty", []byte{}, bytes.Clone(valid[secret.SecretFieldPublicKey]), true},
		{"public missing", bytes.Clone(valid[secret.SecretFieldPrivateKey]), nil, false},
		{"public empty", bytes.Clone(valid[secret.SecretFieldPrivateKey]), []byte{}, false},
		{"both missing", nil, nil, false},
		{"both empty", []byte{}, []byte{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := true
			cr := &v1alpha1.SSHKeyPair{
				TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
				ObjectMeta: metav1.ObjectMeta{Name: "partial", Namespace: "default", UID: "owner-uid"},
				Spec:       v1alpha1.SSHKeyPairSpec{Algorithm: secret.SSHKeyAlgorithmED25519},
			}
			data := map[string][]byte{"unmanaged": []byte("keep")}
			if tt.privateValue != nil {
				data[secret.SecretFieldPrivateKey] = bytes.Clone(tt.privateValue)
			}
			if tt.publicValue != nil {
				data[secret.SecretFieldPublicKey] = bytes.Clone(tt.publicValue)
			}
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "partial", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{Kind: Kind, Controller: &controller}}},
				Data:       data,
			}
			before := existing.DeepCopy()
			c, scheme := sshClient(t, cr, existing)
			r := &ReconcileSSHKeyPair{client: c, scheme: scheme, now: time.Now}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
			if tt.wantErr {
				if err == nil {
					t.Fatal("Reconcile() error = nil, want error")
				}
			} else if err != nil {
				t.Fatal(err)
			}

			got := &corev1.Secret{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), got); err != nil {
				t.Fatal(err)
			}
			if tt.wantErr {
				if !bytes.Equal(got.Data[secret.SecretFieldPublicKey], before.Data[secret.SecretFieldPublicKey]) || !bytes.Equal(got.Data["unmanaged"], before.Data["unmanaged"]) {
					t.Fatal("Secret mutated on impossible private-key repair")
				}
				return
			}
			if len(got.Data[secret.SecretFieldPrivateKey]) == 0 || len(got.Data[secret.SecretFieldPublicKey]) == 0 {
				t.Fatal("missing SSH key field was not repaired")
			}
			if len(before.Data[secret.SecretFieldPrivateKey]) > 0 && !bytes.Equal(got.Data[secret.SecretFieldPrivateKey], before.Data[secret.SecretFieldPrivateKey]) {
				t.Fatal("existing private key changed")
			}
		})
	}
}

func TestSuppliedPrivateKeyRepairPreservesExistingPublicKey(t *testing.T) {
	matching := make(map[string][]byte)
	other := make(map[string][]byte)
	for _, data := range []map[string][]byte{matching, other} {
		if err := secret.GenerateSSHKeypairDataWithAlgorithm(log, secret.SSHKeyAlgorithmED25519, "", secret.SecretFieldPrivateKey, secret.SecretFieldPublicKey, true, data); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name       string
		emptyField bool
		privateKey []byte
		wantErr    bool
	}{
		{"matching missing private", false, matching[secret.SecretFieldPrivateKey], false},
		{"matching empty private", true, matching[secret.SecretFieldPrivateKey], false},
		{"mismatched missing private", false, other[secret.SecretFieldPrivateKey], true},
		{"mismatched empty private", true, other[secret.SecretFieldPrivateKey], true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := true
			cr := &v1alpha1.SSHKeyPair{
				TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
				ObjectMeta: metav1.ObjectMeta{Name: "supplied", Namespace: "default", UID: "owner-uid"},
				Spec:       v1alpha1.SSHKeyPairSpec{PrivateKey: string(tt.privateKey), Algorithm: secret.SSHKeyAlgorithmED25519},
			}
			data := map[string][]byte{secret.SecretFieldPublicKey: bytes.Clone(matching[secret.SecretFieldPublicKey]), "unmanaged": []byte("keep")}
			if tt.emptyField {
				data[secret.SecretFieldPrivateKey] = nil
			}
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "supplied", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{Kind: Kind, Controller: &controller}}},
				Data:       data,
			}
			before := existing.DeepCopy()
			c, scheme := sshClient(t, cr, existing)
			r := &ReconcileSSHKeyPair{client: c, scheme: scheme, now: time.Now}
			_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
			if tt.wantErr {
				if err == nil {
					t.Fatal("Reconcile() error = nil, want error")
				}
			} else if err != nil {
				t.Fatal(err)
			}

			got := &corev1.Secret{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), got); err != nil {
				t.Fatal(err)
			}
			if tt.wantErr {
				if !reflect.DeepEqual(got.Data, before.Data) {
					t.Fatal("mismatched supplied private key mutated Secret data")
				}
				return
			}
			if !bytes.Equal(got.Data[secret.SecretFieldPrivateKey], tt.privateKey) || !bytes.Equal(got.Data[secret.SecretFieldPublicKey], before.Data[secret.SecretFieldPublicKey]) {
				t.Fatal("matching private key was not restored without changing public key")
			}
		})
	}
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
