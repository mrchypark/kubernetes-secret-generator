package secret

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestValidationHelperEdges(t *testing.T) {
	cause := errors.New("cause")
	err := &ValidationError{Field: "field", Err: cause}
	if err.Error() != "invalid field: cause" || !errors.Is(err, cause) {
		t.Fatalf("ValidationError did not format or unwrap: %v", err)
	}
	if Type("unknown").Validate() == nil {
		t.Fatal("unknown Secret type was accepted")
	}
	if _, _, err := ParseByteLength(0, ""); err == nil {
		t.Fatal("invalid fallback length was accepted")
	}
	if _, _, err := ParseByteLength(40, "B"); err == nil {
		t.Fatal("suffix-only length was accepted")
	}

	fieldTests := []struct {
		name      string
		generated []string
		literal   map[string][]byte
	}{
		{name: "empty"},
		{name: "invalid generated", generated: []string{"bad/name"}},
		{name: "duplicate generated", generated: []string{"same", "same"}},
		{name: "invalid literal", literal: map[string][]byte{"bad/name": nil}},
	}
	for _, tt := range fieldTests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateManagedFields(tt.generated, tt.literal); err == nil {
				t.Fatal("invalid managed fields were accepted")
			}
		})
	}

	projected := &corev1.Secret{}
	if err := ValidateProjectedDataSize(projected, map[string]int{"value": 1}); err != nil {
		t.Fatalf("small projected value: %v", err)
	}
	if projected.Data != nil {
		t.Fatal("projection validation mutated the Secret")
	}
	if err := ValidateProjectedDataSize(projected, map[string]int{"value": -1}); err == nil {
		t.Fatal("negative projected length was accepted")
	}
	if err := ValidateProjectedDataSize(projected, map[string]int{"value": MaxProjectedSecretSize}); err == nil {
		t.Fatal("oversized projected value was accepted")
	}
}

func TestEncodedRandomStringAndValidationMatrix(t *testing.T) {
	for _, encoding := range []string{"base64", "base64url", "base32", "hex", "raw"} {
		for _, byteLength := range []bool{false, true} {
			name := encoding
			if byteLength {
				name += "-bytes"
			}
			t.Run(name, func(t *testing.T) {
				value, err := GenerateRandomString(8, encoding, byteLength)
				if err != nil {
					t.Fatalf("GenerateRandomString: %v", err)
				}
				want, err := EncodedOutputLength(8, encoding, byteLength)
				if err != nil {
					t.Fatalf("EncodedOutputLength: %v", err)
				}
				if len(value) != want {
					t.Fatalf("generated length = %d, want %d", len(value), want)
				}
				if err := validateAnnotationString(value, 8, byteLength, encoding); err != nil {
					t.Fatalf("validate generated value: %v", err)
				}
			})
		}
	}
	for _, tt := range []struct {
		name       string
		value      []byte
		length     int
		byteLength bool
		encoding   string
	}{
		{name: "bad length", value: []byte("short"), length: 8, encoding: "base64"},
		{name: "bad character", value: []byte("!!!!"), length: 4, encoding: "base64"},
		{name: "bad base64 bytes", value: []byte("!!!!"), length: 3, byteLength: true, encoding: "base64"},
		{name: "bad base64url bytes", value: []byte("!!!!"), length: 3, byteLength: true, encoding: "base64url"},
		{name: "bad base32 bytes", value: []byte("!!!!!!!!"), length: 5, byteLength: true, encoding: "base32"},
		{name: "bad hex bytes", value: []byte("zzzzzz"), length: 3, byteLength: true, encoding: "hex"},
		{name: "invalid configuration", length: 0, encoding: "raw"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateAnnotationString(tt.value, tt.length, tt.byteLength, tt.encoding); err == nil {
				t.Fatal("invalid generated value was accepted")
			}
		})
	}
	if _, err := EncodedOutputLength(0, "raw", false); err == nil {
		t.Fatal("invalid encoded length was accepted")
	}
	if _, err := EncodedOutputLength(8, "unknown", false); err == nil {
		t.Fatal("invalid encoding was accepted")
	}
}

func validTrackingSecret() *corev1.Secret {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	storeAnnotationTracking(secret, &annotationTracking{
		DataKeys:     []string{"value"},
		Checksums:    map[string]string{"value": annotationDataChecksum("value", []byte("data"))},
		Fingerprints: map[string]string{"value": annotationFingerprint([]byte("spec"))},
	})
	return secret
}

func TestLoadAnnotationTrackingRejectsCorruption(t *testing.T) {
	valid := validTrackingSecret()
	tests := []struct {
		name  string
		state annotationTrackingState
		edit  func(map[string]string)
	}{
		{name: "partial", state: annotationTrackingConflict, edit: func(a map[string]string) { delete(a, AnnotationManagedBy) }},
		{name: "malformed data keys", state: annotationTrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedDataKeys] = "{" }},
		{name: "noncanonical data keys", state: annotationTrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedDataKeys] = `["value","value"]` }},
		{name: "managed labels", state: annotationTrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedLabelKeys] = `["label"]` }},
		{name: "bad digest", state: annotationTrackingConflict, edit: func(a map[string]string) { a[AnnotationManagedKeysDigest] = "bad" }},
		{name: "malformed fingerprints", state: annotationTrackingConflict, edit: func(a map[string]string) { a[AnnotationGenerationFingerprint] = "{" }},
		{name: "nil fingerprints", state: annotationTrackingConflict, edit: func(a map[string]string) { a[AnnotationGenerationFingerprint] = "null" }},
		{name: "bad fingerprint entry", state: annotationTrackingConflict, edit: func(a map[string]string) { a[AnnotationGenerationFingerprint] = `{"value":"bad"}` }},
		{name: "malformed checksums", state: annotationTrackingChecksumIncomplete, edit: func(a map[string]string) { a[AnnotationManagedDataChecksums] = "{" }},
		{name: "bad checksum entry", state: annotationTrackingChecksumIncomplete, edit: func(a map[string]string) { a[AnnotationManagedDataChecksums] = `{"value":"bad"}` }},
		{name: "missing checksum", state: annotationTrackingChecksumIncomplete, edit: func(a map[string]string) { a[AnnotationManagedDataChecksums] = `{}` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := valid.DeepCopy()
			tt.edit(secret.Annotations)
			_, state, err := loadAnnotationTracking(secret)
			if state != tt.state || err == nil {
				t.Fatalf("loadAnnotationTracking = (%v, %v), want state %v and error", state, err, tt.state)
			}
		})
	}
	tracking, state, err := loadAnnotationTracking(valid)
	if err != nil || state != annotationTrackingValid || len(tracking.Checksums) != 1 {
		t.Fatalf("valid tracking rejected: state=%v err=%v", state, err)
	}
}

func TestBuildAnnotationPlanValidation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
	}{
		{name: "unknown type", annotations: map[string]string{AnnotationSecretType: "unknown"}},
		{name: "empty string fields", annotations: map[string]string{AnnotationSecretType: string(TypeString)}},
		{name: "string length", annotations: map[string]string{AnnotationSecretAutoGenerate: "value", AnnotationSecretLength: "bad"}},
		{name: "string encoding", annotations: map[string]string{AnnotationSecretAutoGenerate: "value", AnnotationSecretEncoding: "bad"}},
		{name: "basic username", annotations: map[string]string{AnnotationSecretType: string(TypeBasicAuth), AnnotationBasicAuthUsername: "bad:name"}},
		{name: "ssh private field", annotations: map[string]string{AnnotationSecretType: string(TypeSSHKeypair), AnnotationSSHPrivateKeyField: ""}},
		{name: "ssh public field", annotations: map[string]string{AnnotationSecretType: string(TypeSSHKeypair), AnnotationSSHPublicKeyField: ""}},
		{name: "ssh colliding fields", annotations: map[string]string{AnnotationSecretType: string(TypeSSHKeypair), AnnotationSSHPrivateKeyField: "same", AnnotationSSHPublicKeyField: "same"}},
		{name: "ssh algorithm", annotations: map[string]string{AnnotationSecretType: string(TypeSSHKeypair), AnnotationSSHKeyAlgorithm: "unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildAnnotationPlan(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}})
			if !IsValidationError(err) {
				t.Fatalf("buildAnnotationPlan error = %v, want ValidationError", err)
			}
		})
	}
	for _, algorithm := range []string{SSHKeyAlgorithmECDSA, SSHKeyAlgorithmED25519} {
		plan, err := buildAnnotationPlan(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			AnnotationSecretType:      string(TypeSSHKeypair),
			AnnotationSSHKeyAlgorithm: algorithm,
		}}})
		if err != nil {
			t.Fatalf("default %s plan: %v", algorithm, err)
		}
		if plan == nil || plan.algorithm != algorithm {
			t.Fatalf("default %s plan: planNil=%t algorithmMatches=%t", algorithm, plan == nil, plan != nil && plan.algorithm == algorithm)
		}
	}
}

func TestBasicAuthValidationFailures(t *testing.T) {
	if err := ValidateBasicAuthUsername(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 username was accepted")
	}
	if err := ValidateBasicAuthLiteralData(nil); err != nil {
		t.Fatalf("empty literal data: %v", err)
	}
	if err := ValidateBasicAuthLiteralData(map[string][]byte{FieldBasicAuthPassword: nil}); err == nil {
		t.Fatal("reserved BasicAuth field was accepted")
	}
	if err := ValidateBasicAuthCredential(map[string][]byte{
		FieldBasicAuthUsername: []byte("user"), FieldBasicAuthIngress: []byte("other:hash"),
	}); err == nil {
		t.Fatal("mismatched BasicAuth username was accepted")
	}
	if err := ValidateBasicAuthCredential(map[string][]byte{
		FieldBasicAuthUsername: []byte("user"), FieldBasicAuthPassword: []byte("password"), FieldBasicAuthIngress: []byte("user:not-a-hash"),
	}); err == nil {
		t.Fatal("invalid BasicAuth hash was accepted")
	}
	for _, cons := range []*BasicAuthConstraints{
		{Username: "bad:name", Encoding: "raw", Length: "8"},
		{Username: "user", Encoding: "bad", Length: "8"},
		{Username: "user", Encoding: "raw", Length: "bad"},
	} {
		if _, err := BasicAuthProjectedValueLengths(cons); err == nil {
			t.Fatal("invalid BasicAuth constraints were accepted")
		}
	}
}

func TestGeneratorFailurePathsAndWriteIntent(t *testing.T) {
	validBasic := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			Data:       map[string][]byte{FieldBasicAuthIngress: []byte("existing")},
		}
	}
	for _, tt := range []struct {
		name string
		edit func(*corev1.Secret)
	}{
		{name: "regenerate", edit: func(s *corev1.Secret) { s.Annotations[AnnotationSecretRegenerate] = "unknown" }},
		{name: "length", edit: func(s *corev1.Secret) { s.Annotations[AnnotationSecretLength] = "bad" }},
		{name: "encoding", edit: func(s *corev1.Secret) { s.Annotations[AnnotationSecretEncoding] = "bad" }},
		{name: "projected size", edit: func(s *corev1.Secret) { s.Annotations["padding"] = strings.Repeat("x", MaxProjectedSecretSize) }},
	} {
		t.Run("basic-"+tt.name, func(t *testing.T) {
			secret := validBasic()
			tt.edit(secret)
			if _, err := (BasicAuthGenerator{}).generateData(secret); err == nil {
				t.Fatal("invalid BasicAuth generator input was accepted")
			}
		})
	}
	stable := validBasic()
	stable.Annotations[AnnotationSecretRegenerate] = "no"
	before := append([]byte(nil), stable.Data[FieldBasicAuthIngress]...)
	if _, err := (BasicAuthGenerator{}).generateData(stable); err != nil {
		t.Fatalf("stable BasicAuth generator: %v", err)
	}
	if !bytes.Equal(before, stable.Data[FieldBasicAuthIngress]) || stable.Annotations[AnnotationSecretRegenerate] != "" {
		t.Fatal("no-op BasicAuth request changed credential data or remained pending")
	}

	for _, tt := range []struct {
		name        string
		annotations map[string]string
	}{
		{name: "fields", annotations: map[string]string{AnnotationSecretAutoGenerate: ""}},
		{name: "regenerate", annotations: map[string]string{AnnotationSecretAutoGenerate: "value", AnnotationSecretRegenerate: "unknown"}},
		{name: "length", annotations: map[string]string{AnnotationSecretAutoGenerate: "value", AnnotationSecretLength: "bad"}},
		{name: "encoding", annotations: map[string]string{AnnotationSecretAutoGenerate: "value", AnnotationSecretEncoding: "bad"}},
	} {
		t.Run("string-"+tt.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}, Data: map[string][]byte{}}
			if _, err := (StringGenerator{}).generateData(secret); err == nil {
				t.Fatal("invalid string generator input was accepted")
			}
		})
	}
	if _, err := GenerateRandomString(0, "raw", false); err == nil {
		t.Fatal("zero-length random string was generated")
	}
	if _, err := GenerateRandomString(8, "unknown", false); err == nil {
		t.Fatal("random string used unsupported encoding")
	}
	if !contains([]string{"first", "second"}, "second") || contains([]string{"first"}, "missing") {
		t.Fatal("generated field membership check failed")
	}

	for _, annotations := range []map[string]string{
		{AnnotationSecretRegenerate: "unknown"},
		{AnnotationSSHPrivateKeyField: ""},
		{AnnotationSSHPublicKeyField: ""},
		{AnnotationSSHPrivateKeyField: "same", AnnotationSSHPublicKeyField: "same"},
		{AnnotationSSHKeyAlgorithm: "unknown"},
	} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}, Data: map[string][]byte{}}
		if _, err := (SSHKeypairGenerator{}).generateData(secret); err == nil {
			t.Fatal("invalid SSH generator input was accepted")
		}
	}
}

func makePrivateKeyPEMs(t *testing.T) (rsaPEM, ecdsaPEM, ed25519PEM []byte) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, ed25519Key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key    interface{}
		target *[]byte
	}{{rsaKey, &rsaPEM}, {ecdsaKey, &ecdsaPEM}, {ed25519Key, &ed25519PEM}} {
		*item.target, err = pemBytesForPrivateKey(item.key)
		if err != nil {
			t.Fatal(err)
		}
	}
	return rsaPEM, ecdsaPEM, ed25519PEM
}

func TestSSHValidationFailureMatrix(t *testing.T) {
	for _, tt := range []struct {
		private, public string
		literal         map[string][]byte
	}{
		{private: "", public: "public"},
		{private: "private", public: ""},
		{private: "same", public: "same"},
		{private: "private", public: "public", literal: map[string][]byte{"public": nil}},
	} {
		if err := ValidateSSHFields(tt.private, tt.public, tt.literal); err == nil {
			t.Fatal("invalid SSH fields were accepted")
		}
	}
	for _, bits := range []int{384, 521} {
		if _, err := ecdsaCurve(bits); err != nil {
			t.Fatalf("ecdsaCurve(%d): %v", bits, err)
		}
	}
	if _, err := ecdsaCurve(255); err == nil {
		t.Fatal("unsupported ECDSA curve was accepted")
	}
	if _, err := pemBytesForPrivateKey(struct{}{}); err == nil {
		t.Fatal("unsupported private key type was encoded")
	}
	if _, err := rawPrivateKeyFromPEM(nil); err == nil {
		t.Fatal("empty private key was accepted")
	}
	if _, err := rawPrivateKeyFromPEM(bytes.Repeat([]byte{'x'}, MaxPrivateKeyPEMBytes+1)); err == nil {
		t.Fatal("oversized private key was accepted")
	}

	rsaPEM, ecdsaPEM, ed25519PEM := makePrivateKeyPEMs(t)
	checks := []struct {
		name      string
		pem       []byte
		algorithm string
		length    int
	}{
		{name: "rsa strength", pem: rsaPEM, algorithm: SSHKeyAlgorithmRSA, length: 3072},
		{name: "rsa as ecdsa", pem: rsaPEM, algorithm: SSHKeyAlgorithmECDSA, length: 256},
		{name: "rsa as ed25519", pem: rsaPEM, algorithm: SSHKeyAlgorithmED25519},
		{name: "ecdsa strength", pem: ecdsaPEM, algorithm: SSHKeyAlgorithmECDSA, length: 384},
		{name: "ecdsa as rsa", pem: ecdsaPEM, algorithm: SSHKeyAlgorithmRSA, length: 2048},
		{name: "ed25519 as rsa", pem: ed25519PEM, algorithm: SSHKeyAlgorithmRSA, length: 2048},
		{name: "unknown algorithm", pem: rsaPEM, algorithm: "unknown"},
	}
	for _, tt := range checks {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ValidateSSHPrivateKey(tt.pem, tt.algorithm, tt.length); err == nil {
				t.Fatal("mismatched private key was accepted")
			}
		})
	}
	if _, err := ValidateSSHPrivateKey([]byte("bad PEM"), SSHKeyAlgorithmRSA, 2048); err == nil {
		t.Fatal("malformed PEM was accepted")
	}
	if _, err := PrivateKeyFromPEM(ecdsaPEM); err == nil {
		t.Fatal("ECDSA key was returned as RSA")
	}
	if _, err := SSHPublicKeyForPrivateKey(struct{}{}); err == nil {
		t.Fatal("unsupported public key input was accepted")
	}
	if err := CheckAndRegenPublicKey(map[string][]byte{}, nil, []byte("bad PEM"), "public"); err == nil {
		t.Fatal("malformed private key repaired a public key")
	}
	if err := checkAndRepairPublicKey(map[string][]byte{}, nil, struct{}{}, "public"); err == nil {
		t.Fatal("unsupported private key repaired a public key")
	}
	if projected, err := SSHProjectedValueLengths(SSHKeyAlgorithmECDSA, 384, "private", "public", true, nil); err != nil || projected["private"] != 768 {
		t.Fatalf("ECDSA projection = %v, %v", projected, err)
	}
	if projected, err := SSHProjectedValueLengths(SSHKeyAlgorithmED25519, 0, "private", "public", true, nil); err != nil || projected["private"] != 512 {
		t.Fatalf("Ed25519 projection = %v, %v", projected, err)
	}

	stringPlan := &annotationPlan{kind: TypeString, fields: []string{"value"}, length: 8, encoding: "base64", regenerate: map[string]bool{}}
	if _, _, invalid := stringPlan.baselineRotation(&corev1.Secret{Data: map[string][]byte{"value": []byte("invalid!")}}); !invalid {
		t.Fatal("invalid legacy string baseline was accepted")
	}
	stringPlan.regenerate["value"] = true
	if _, _, invalid := stringPlan.baselineRotation(&corev1.Secret{Data: map[string][]byte{"value": []byte("invalid!")}}); invalid {
		t.Fatal("explicit string rotation did not bypass legacy validation")
	}
	basicPlan := &annotationPlan{kind: TypeBasicAuth, fields: []string{FieldBasicAuthIngress, FieldBasicAuthPassword, FieldBasicAuthUsername}, username: "user", length: 8, encoding: "raw", regenerate: map[string]bool{}}
	badBasic := &corev1.Secret{Data: map[string][]byte{FieldBasicAuthIngress: []byte("user:bad"), FieldBasicAuthPassword: []byte("password"), FieldBasicAuthUsername: []byte("user")}}
	if _, _, invalid := basicPlan.baselineRotation(badBasic); !invalid {
		t.Fatal("invalid legacy BasicAuth baseline was accepted")
	}
	basicPlan.regenerate[FieldBasicAuthPassword] = true
	if _, _, invalid := basicPlan.baselineRotation(badBasic); invalid {
		t.Fatal("explicit BasicAuth rotation did not bypass legacy validation")
	}
	sshPlan := &annotationPlan{kind: TypeSSHKeypair, privateField: "private", publicField: "public", algorithm: SSHKeyAlgorithmRSA, sshLength: 2048, regenerate: map[string]bool{}}
	badSSH := &corev1.Secret{Data: map[string][]byte{"private": []byte("bad PEM")}}
	if _, _, invalid := sshPlan.baselineRotation(badSSH); !invalid {
		t.Fatal("invalid legacy SSH baseline was accepted")
	}
	sshPlan.regenerate["private"] = true
	if _, _, invalid := sshPlan.baselineRotation(badSSH); invalid {
		t.Fatal("explicit SSH rotation did not bypass legacy validation")
	}
}

type fixedGetClient struct {
	client.Client
	err error
}

func (c fixedGetClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return c.err
}

func TestReconcileReadFailuresAndControllerHelpers(t *testing.T) {
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "missing"}}
	for _, tt := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "not found", err: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, request.Name)},
		{name: "read error", err: errors.New("read failed"), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (&ReconcileSecret{client: fixedGetClient{err: tt.err}}).Reconcile(context.Background(), request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	for reason, want := range map[string]string{
		reasonLegacyBaselineInvalid:   "Existing generated data does not match the effective configuration",
		reasonTrackingStateConflict:   "Secret generator tracking state is inconsistent",
		reasonSecretSizeConflict:      "Projected Secret size exceeds the supported limit",
		reasonImmutableSecretConflict: "Immutable Secret requires reconciliation and was left unchanged",
		"other":                       "Secret generator configuration is invalid",
	} {
		if got := annotationTerminalMessage(reason); got != want {
			t.Fatalf("annotationTerminalMessage(%q) = %q, want %q", reason, got, want)
		}
	}
	if annotationSuccessMessage(true) == annotationSuccessMessage(false) {
		t.Fatal("rotated and stable success messages are identical")
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "secret"}}
	if isAnnotationManaged(nil) || isAnnotationManaged(secret) {
		t.Fatal("unmanaged Secret was classified as managed")
	}
	if annotationTransitionKey(secret) != "test/secret" {
		t.Fatal("transition key did not fall back to namespace/name")
	}
	secret.UID = "uid"
	if annotationTransitionKey(secret) != "uid" {
		t.Fatal("transition key did not use UID")
	}

	annotationExpectedWrites.Lock()
	annotationExpectedWrites.resourceVersions = map[string]annotationExpectedWrite{}
	annotationExpectedWrites.Unlock()
	t.Cleanup(func() {
		annotationExpectedWrites.Lock()
		annotationExpectedWrites.resourceVersions = map[string]annotationExpectedWrite{}
		annotationExpectedWrites.Unlock()
	})
	rememberAnnotationWrite(secret)
	secret.ResourceVersion = "1"
	rememberAnnotationWrite(secret)
	annotationExpectedWrites.Lock()
	expected := annotationExpectedWrites.resourceVersions[annotationObjectKey(secret)]
	expected.expires = time.Now().Add(-time.Second)
	annotationExpectedWrites.resourceVersions[annotationObjectKey(secret)] = expected
	annotationExpectedWrites.Unlock()
	if consumeAnnotationWrite(secret) {
		t.Fatal("expired expected write was consumed")
	}

	annotationEventTransitions.Lock()
	annotationEventTransitions.last = make(map[string]string, maxAnnotationTransitionKeys)
	for i := 0; i < maxAnnotationTransitionKeys; i++ {
		annotationEventTransitions.last[string(rune(i+1))] = "old"
	}
	annotationEventTransitions.Unlock()
	(&ReconcileSecret{}).markTransition(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{UID: "new-uid"}}, reasonReconciled)
	annotationEventTransitions.Lock()
	transitionCount := len(annotationEventTransitions.last)
	_, retained := annotationEventTransitions.last["new-uid"]
	annotationEventTransitions.last = map[string]string{}
	annotationEventTransitions.Unlock()
	if transitionCount != maxAnnotationTransitionKeys || !retained {
		t.Fatal("bounded transition cache did not evict an old entry")
	}
}

func TestAnnotationPredicateCreateDelete(t *testing.T) {
	predicate := annotationSecretPredicate()
	unmanaged := &corev1.Secret{}
	if predicate.Create(event.TypedCreateEvent[*corev1.Secret]{Object: unmanaged}) {
		t.Fatal("unmanaged create was enqueued")
	}
	managed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{UID: "managed", Annotations: map[string]string{AnnotationSecretAutoGenerate: "value"}}}
	if !predicate.Create(event.TypedCreateEvent[*corev1.Secret]{Object: managed}) {
		t.Fatal("managed create was not enqueued")
	}
	if predicate.Delete(event.TypedDeleteEvent[*corev1.Secret]{Object: unmanaged}) {
		t.Fatal("unmanaged delete was enqueued")
	}
	rememberAnnotationWrite(managed)
	if !predicate.Delete(event.TypedDeleteEvent[*corev1.Secret]{Object: managed}) {
		t.Fatal("managed delete was not enqueued")
	}
	if predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: unmanaged, ObjectNew: unmanaged.DeepCopy()}) {
		t.Fatal("unmanaged update was enqueued")
	}
}

func TestSmallAnnotationHelpers(t *testing.T) {
	secret := &corev1.Secret{}
	storeAnnotationTracking(secret, &annotationTracking{Checksums: map[string]string{}, Fingerprints: map[string]string{}})
	if secret.Annotations == nil {
		t.Fatal("tracking store did not initialize annotations")
	}
	if missingAny(map[string][]byte{"a": []byte("value")}, []string{"a"}) {
		t.Fatal("present field reported missing")
	}
	for _, values := range [][]string{nil, {""}, {"b", "a"}} {
		if canonicalAnnotationList(values) {
			t.Fatalf("noncanonical list accepted: %v", values)
		}
	}
	if validAnnotationSHA("short") || validAnnotationSHA(strings.Repeat("z", 64)) {
		t.Fatal("invalid checksum was accepted")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("mustAnnotationJSON did not panic on unsupported value")
		}
	}()
	_ = mustAnnotationJSON(func() {})
}
