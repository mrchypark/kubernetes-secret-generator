package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

func TestGeneratedValueValidation(t *testing.T) {
	for _, tc := range []struct {
		value, length, encoding string
		ok                      bool
	}{
		{"abcdefgh", "8", "raw", true},
		{"61626364", "4B", "hex", true},
		{"YWJjZA==", "4B", "base64", true},
		{"not-hex!", "8", "hex", false},
		{"short", "8", "raw", false},
	} {
		if got := validateGenerated([]byte(tc.value), tc.length, tc.encoding, 40) == nil; got != tc.ok {
			t.Fatalf("validateGenerated(%q, %q, %q) = %t, want %t", tc.value, tc.length, tc.encoding, got, tc.ok)
		}
	}
}

func TestBasicAuthBaseline(t *testing.T) {
	password := []byte("abcdefgh")
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{"username": []byte("user"), "password": password, "auth": append([]byte("user:"), hash...)}
	if err := validateBasicAuth(data, "user", "8", "raw", 40); err != nil {
		t.Fatal(err)
	}
	data["password"] = []byte("ijklmnop")
	if err := validateBasicAuth(data, "user", "8", "raw", 40); err == nil {
		t.Fatal("accepted mismatched bcrypt password")
	}
}

func TestAnnotationValidationUsesEffectiveDefaultsAndRejectsTypeAndRuneOverflow(t *testing.T) {
	password := []byte("abcdefgh")
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value []byte) string { return base64.StdEncoding.EncodeToString(value) }
	valid := object{Kind: "Secret", Metadata: metadata{Name: "auth", Annotations: map[string]string{annotationPrefix + "type": "basic-auth", annotationPrefix + "basic-auth-username": "user"}}, Data: map[string]string{
		"username": encode([]byte("user")), "password": encode(password), "auth": encode(append([]byte("user:"), hash...)),
	}}
	if findings := validateAnnotation(valid, defaults{Length: 8, Encoding: "raw", SSHAlgorithm: "rsa", SSHLength: 2048}); len(findings) != 0 {
		t.Fatalf("valid defaults findings: %+v", findings)
	}
	unknown := valid
	unknown.Metadata.Annotations = map[string]string{annotationPrefix + "type": "mystery"}
	if got := validateAnnotation(unknown, defaults{}); len(got) != 1 || got[0].Code != "InvalidAnnotationType" {
		t.Fatalf("unknown type findings: %+v", got)
	}
	overflow := valid
	overflow.Metadata.Annotations = map[string]string{annotationPrefix + "type": "basic-auth", annotationPrefix + "basic-auth-username": strings.Repeat("가", 256)}
	if got := validateAnnotation(overflow, defaults{Length: 8, Encoding: "raw"}); len(got) != 1 || got[0].Code != "InvalidUsername" {
		t.Fatalf("overflow findings: %+v", got)
	}
}

func TestSSHBaseline(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	publicBytes := ssh.MarshalAuthorizedKey(sshPublic)
	if err := validateSSH(privatePEM, publicBytes, "ed25519", 0, privatePEM); err != nil {
		t.Fatal(err)
	}
	if err := validateSSH(privatePEM, []byte("ssh-ed25519 invalid\n"), "ed25519", 0, privatePEM); err == nil {
		t.Fatal("accepted mismatched public key")
	}
}

func TestTypeMismatchIsInventoriedRegardlessOfImmutable(t *testing.T) {
	cr := object{Kind: "StringSecret", Metadata: metadata{Name: "typed"}, Spec: spec{Type: "example.test/desired"}}
	for _, immutable := range []bool{false, true} {
		secret := object{Kind: "Secret", Type: "example.test/actual", Immutable: immutable, Data: map[string]string{}}
		got := validateCR(cr, secret, defaults{})
		if len(got) != 1 || got[0].Code != "SecretTypeMismatch" {
			t.Fatalf("immutable=%v findings: %+v", immutable, got)
		}
	}
}

func TestValidateRedactsSecretValues(t *testing.T) {
	secretValue := "do-not-emit"
	in := input{Deployment: json.RawMessage(`{"spec":{"template":{"spec":{"containers":[{"name":"kubernetes-secret-generator","env":[{"name":"WATCH_NAMESPACE","value":"app"}]}]}}}}`), Items: []object{
		{APIVersion: "secretgenerator.mittwald.de/v1alpha1", Kind: "StringSecret", Metadata: metadata{Namespace: "app", Name: "login", UID: "uid-1"}, Spec: spec{Fields: []field{{FieldName: "password", Length: "8", Encoding: "raw"}}}},
		{Kind: "Secret", Metadata: metadata{Namespace: "app", Name: "login", OwnerReferences: []ownerReference{{APIVersion: "secretgenerator.mittwald.de/v1alpha1", Kind: "StringSecret", Name: "login", UID: "uid-1", Controller: true}}}, Type: "Opaque", Data: map[string]string{"password": base64.StdEncoding.EncodeToString([]byte(secretValue))}},
	}}
	validated, err := validate(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := jsonMarshal(validated)
	if err != nil {
		t.Fatal(err)
	}
	if stringContains(string(b), secretValue) {
		t.Fatal("finding exposed Secret value")
	}
}

func TestDeploymentDefaultsSelectManagerAndDoNotConsumeBooleanFollower(t *testing.T) {
	raw := json.RawMessage(`{"spec":{"template":{"spec":{"containers":[
		{"name":"sidecar","args":["--secret-length=999"]},
		{"name":"kubernetes-secret-generator","args":["--disable-crd-support","--secret-length","48","--leader-elect=true","--secret-encoding=hex"],"env":[{"name":"WATCH_NAMESPACE","value":"app"}]}
	]}}}}`)
	d, err := deploymentDefaults(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.Length != 48 || d.Encoding != "hex" {
		t.Fatalf("defaults = %+v", d)
	}
}

// Kept tiny to avoid assertion dependencies in this standalone helper module.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
func stringContains(s, sub string) bool { return strings.Contains(s, sub) }
