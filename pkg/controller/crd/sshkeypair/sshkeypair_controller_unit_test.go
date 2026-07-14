package sshkeypair

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/pem"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
	golangssh "golang.org/x/crypto/ssh"
)

func TestEd25519SeedValidationAndDeterministicPEM(t *testing.T) {
	seedBytes := testEd25519SeedBytes(0)
	encoded := base64.StdEncoding.EncodeToString(seedBytes)
	first, err := secret.PEMPrivateKeyFromEd25519Seed(encoded)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secret.PEMPrivateKeyFromEd25519Seed(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same seed produced different private keys")
	}
	block, rest := pem.Decode(first)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		t.Fatal("seed did not produce one PKCS#8 PRIVATE KEY PEM block")
	}
	raw, err := golangssh.ParseRawPrivateKey(first)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := raw.(ed25519.PrivateKey)
	if !ok || !bytes.Equal(key.Seed(), seedBytes) {
		t.Fatal("PEM does not contain the supplied Ed25519 seed")
	}

	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := strings.IndexByte(alphabet, encoded[len(encoded)-2])
	noncanonical := encoded[:len(encoded)-2] + string(alphabet[last+1]) + "="
	tests := []struct {
		name  string
		value string
	}{
		{"malformed", "not-base64!"},
		{"oversized", strings.Repeat("A", 1024)},
		{"unpadded", strings.TrimSuffix(encoded, "=")},
		{"noncanonical", noncanonical},
		{"31 bytes", base64.StdEncoding.EncodeToString(seedBytes[:31])},
		{"33 bytes", base64.StdEncoding.EncodeToString(append(bytes.Clone(seedBytes), 32))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := secret.PEMPrivateKeyFromEd25519Seed(tt.value)
			if err == nil {
				t.Fatal("PEMPrivateKeyFromEd25519Seed() error = nil, want error")
			}
			if strings.Contains(err.Error(), tt.value) {
				t.Fatal("validation error exposed seed input")
			}
		})
	}
}

func TestEd25519SeedSpecConflicts(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(testEd25519SeedBytes(0))
	tests := []struct {
		name string
		spec v1alpha1.SSHKeyPairSpec
	}{
		{"private key conflict", v1alpha1.SSHKeyPairSpec{PrivateKey: "pem", Ed25519Seed: seed}},
		{"algorithm conflict", v1alpha1.SSHKeyPairSpec{Algorithm: secret.SSHKeyAlgorithmRSA, Ed25519Seed: seed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveSuppliedPrivateKey(&v1alpha1.SSHKeyPair{Spec: tt.spec})
			if err == nil {
				t.Fatal("resolveSuppliedPrivateKey() error = nil, want error")
			}
			if strings.Contains(err.Error(), seed) {
				t.Fatal("validation error exposed seed input")
			}
		})
	}

	r := &ReconcileSSHKeyPair{now: time.Now}
	_, _, err := r.scheduleRotation(&v1alpha1.SSHKeyPair{Spec: v1alpha1.SSHKeyPairSpec{Ed25519Seed: seed, RotationInterval: "1h"}}, &corev1.Secret{})
	if err == nil {
		t.Fatal("seed with rotationInterval was accepted")
	}
}

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

func TestEd25519SeedCreateUpdateAndForce(t *testing.T) {
	seed, suppliedPrivate, suppliedPublic := seedKeyMaterial(t, 0)
	_, existingPrivate, existingPublic := seedKeyMaterial(t, 64)
	tests := []struct {
		name        string
		existing    bool
		force       bool
		wantPrivate []byte
		wantPublic  []byte
	}{
		{"create with empty algorithm", false, false, suppliedPrivate, suppliedPublic},
		{"update preserves existing key", true, false, existingPrivate, existingPublic},
		{"force uses supplied seed", true, true, suppliedPrivate, suppliedPublic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := true
			cr := &v1alpha1.SSHKeyPair{
				TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
				ObjectMeta: metav1.ObjectMeta{Name: "seed-lifecycle", Namespace: "default", UID: "owner-uid"},
				Spec:       v1alpha1.SSHKeyPairSpec{Ed25519Seed: seed, ForceRegenerate: tt.force},
			}
			objects := []client.Object{cr}
			if tt.existing {
				objects = append(objects, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: cr.Name, Namespace: cr.Namespace, OwnerReferences: []metav1.OwnerReference{{Kind: Kind, Controller: &controller}}},
					Data:       map[string][]byte{secret.SecretFieldPrivateKey: bytes.Clone(existingPrivate), secret.SecretFieldPublicKey: bytes.Clone(existingPublic)},
				})
			}
			c, scheme := sshClient(t, objects...)
			r := &ReconcileSSHKeyPair{client: c, scheme: scheme, now: time.Now}
			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err != nil {
				t.Fatal(err)
			}
			got := &corev1.Secret{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(cr), got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data[secret.SecretFieldPrivateKey], tt.wantPrivate) || !bytes.Equal(got.Data[secret.SecretFieldPublicKey], tt.wantPublic) {
				t.Fatal("seed lifecycle produced unexpected key material")
			}
		})
	}
}

func TestSuppliedKeySourceIgnoresManagedFieldData(t *testing.T) {
	seed, suppliedPrivate, suppliedPublic := seedKeyMaterial(t, 0)
	_, existingPrivate, existingPublic := seedKeyMaterial(t, 64)
	tests := []struct {
		name            string
		spec            v1alpha1.SSHKeyPairSpec
		existing        bool
		existingData    map[string][]byte
		privateKeyField string
		publicKeyField  string
	}{
		{
			name: "seed create with default fields",
			spec: v1alpha1.SSHKeyPairSpec{
				Ed25519Seed: seed,
				Data: map[string]string{
					secret.SecretFieldPrivateKey: "ignored private literal",
					secret.SecretFieldPublicKey:  "ignored public literal",
					"purpose":                    "keep",
				},
			},
			privateKeyField: secret.SecretFieldPrivateKey,
			publicKeyField:  secret.SecretFieldPublicKey,
		},
		{
			name: "seed repairs both missing fields",
			spec: v1alpha1.SSHKeyPairSpec{
				Ed25519Seed: seed,
				Data: map[string]string{
					secret.SecretFieldPrivateKey: "ignored private literal",
					secret.SecretFieldPublicKey:  "ignored public literal",
					"purpose":                    "keep",
				},
			},
			existing:        true,
			existingData:    map[string][]byte{},
			privateKeyField: secret.SecretFieldPrivateKey,
			publicKeyField:  secret.SecretFieldPublicKey,
		},
		{
			name: "seed force update",
			spec: v1alpha1.SSHKeyPairSpec{
				Ed25519Seed:     seed,
				ForceRegenerate: true,
				Data: map[string]string{
					secret.SecretFieldPrivateKey: "ignored private literal",
					secret.SecretFieldPublicKey:  "ignored public literal",
					"purpose":                    "keep",
				},
			},
			existing: true,
			existingData: map[string][]byte{
				secret.SecretFieldPrivateKey: bytes.Clone(existingPrivate),
				secret.SecretFieldPublicKey:  bytes.Clone(existingPublic),
			},
			privateKeyField: secret.SecretFieldPrivateKey,
			publicKeyField:  secret.SecretFieldPublicKey,
		},
		{
			name: "seed create with custom fields",
			spec: v1alpha1.SSHKeyPairSpec{
				Ed25519Seed:     seed,
				PrivateKeyField: "id_ed25519",
				PublicKeyField:  "id_ed25519.pub",
				Data: map[string]string{
					"id_ed25519":     "ignored private literal",
					"id_ed25519.pub": "ignored public literal",
					"purpose":        "keep",
				},
			},
			privateKeyField: "id_ed25519",
			publicKeyField:  "id_ed25519.pub",
		},
		{
			name: "privateKey create with default fields",
			spec: v1alpha1.SSHKeyPairSpec{
				Algorithm:  secret.SSHKeyAlgorithmED25519,
				PrivateKey: string(suppliedPrivate),
				Data: map[string]string{
					secret.SecretFieldPrivateKey: "ignored private literal",
					secret.SecretFieldPublicKey:  "ignored public literal",
					"purpose":                    "keep",
				},
			},
			privateKeyField: secret.SecretFieldPrivateKey,
			publicKeyField:  secret.SecretFieldPublicKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := true
			cr := &v1alpha1.SSHKeyPair{
				TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
				ObjectMeta: metav1.ObjectMeta{Name: "managed-data-collision", Namespace: "default", UID: "owner-uid"},
				Spec:       tt.spec,
			}
			objects := []client.Object{cr}
			if tt.existing {
				objects = append(objects, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: cr.Name, Namespace: cr.Namespace, OwnerReferences: []metav1.OwnerReference{{Kind: Kind, Controller: &controller}}},
					Data:       tt.existingData,
				})
			}
			c, scheme := sshClient(t, objects...)
			r := &ReconcileSSHKeyPair{client: c, scheme: scheme, now: time.Now}
			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err != nil {
				t.Fatal(err)
			}
			got := &corev1.Secret{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(cr), got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data[tt.privateKeyField], suppliedPrivate) {
				t.Fatal("managed private-key literal overrode the supplied key source")
			}
			if !bytes.Equal(got.Data[tt.publicKeyField], suppliedPublic) {
				t.Fatal("managed public-key literal prevented derivation from the supplied key source")
			}
			if string(got.Data["purpose"]) != "keep" {
				t.Fatal("unrelated literal data was not preserved")
			}
		})
	}
}

func TestEd25519SeedPartialRepairMatchesExistingPublicKey(t *testing.T) {
	matchingSeed, matchingPrivate, matchingPublic := seedKeyMaterial(t, 0)
	otherSeed, _, _ := seedKeyMaterial(t, 64)
	tests := []struct {
		name       string
		seed       string
		emptyField bool
		wantErr    bool
	}{
		{"matching missing private", matchingSeed, false, false},
		{"matching empty private", matchingSeed, true, false},
		{"mismatched missing private", otherSeed, false, true},
		{"mismatched empty private", otherSeed, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := true
			cr := &v1alpha1.SSHKeyPair{
				TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
				ObjectMeta: metav1.ObjectMeta{Name: "seed-partial", Namespace: "default", UID: "owner-uid"},
				Spec:       v1alpha1.SSHKeyPairSpec{Ed25519Seed: tt.seed},
			}
			data := map[string][]byte{secret.SecretFieldPublicKey: bytes.Clone(matchingPublic), "unmanaged": []byte("keep")}
			if tt.emptyField {
				data[secret.SecretFieldPrivateKey] = nil
			}
			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: cr.Name, Namespace: cr.Namespace, OwnerReferences: []metav1.OwnerReference{{Kind: Kind, Controller: &controller}}},
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
					t.Fatal("mismatched seed mutated Secret data")
				}
				return
			}
			if !bytes.Equal(got.Data[secret.SecretFieldPrivateKey], matchingPrivate) || !bytes.Equal(got.Data[secret.SecretFieldPublicKey], matchingPublic) {
				t.Fatal("matching seed was not repaired without changing public key")
			}
		})
	}
}

func testEd25519SeedBytes(start byte) []byte {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = start + byte(index)
	}
	return seed
}

func seedKeyMaterial(t *testing.T, start byte) (string, []byte, []byte) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(testEd25519SeedBytes(start))
	privateKey, err := secret.PEMPrivateKeyFromEd25519Seed(encoded)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{secret.SecretFieldPrivateKey: bytes.Clone(privateKey)}
	if err := secret.CheckAndRegenPublicKey(data, nil, privateKey, secret.SecretFieldPublicKey); err != nil {
		t.Fatal(err)
	}
	return encoded, privateKey, data[secret.SecretFieldPublicKey]
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
	if err := v1alpha1.SchemeBuilder.AddToScheme(clientgoscheme.Scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.SSHKeyPair{}).WithObjects(objects...).Build(), scheme
}
