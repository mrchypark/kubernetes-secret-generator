package secret

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAnnotationGeneratorsAcceptValidConfiguration(t *testing.T) {
	stringSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationSecretAutoGenerate: "value",
	}}, Data: map[string][]byte{}}
	if _, err := (StringGenerator{}).generateData(stringSecret); err != nil {
		t.Fatalf("generate string Secret: %v", err)
	}
	basicSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}, Data: map[string][]byte{}}
	if _, err := (BasicAuthGenerator{}).generateData(basicSecret); err != nil {
		t.Fatalf("generate BasicAuth Secret: %v", err)
	}
}

func TestParseByteLengthStrict(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		bytes   bool
		wantErr bool
	}{
		{"", 40, false, false},
		{"1", 1, false, false},
		{"65536B", 65536, true, false},
		{"65536b", 65536, true, false},
		{"0", 0, false, true},
		{"-1", 0, false, true},
		{"+1", 0, false, true},
		{" 1", 0, false, true},
		{"1KB", 0, false, true},
		{"65537", 0, false, true},
		{"999999999999999999999999", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, gotBytes, err := ParseByteLength(40, tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseByteLength() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (got != tt.want || gotBytes != tt.bytes) {
				t.Fatalf("ParseByteLength() = (%d, %v), want (%d, %v)", got, gotBytes, tt.want, tt.bytes)
			}
		})
	}
}

func TestParseRegenerate(t *testing.T) {
	generated := []string{"one", "two"}
	tests := []struct {
		value     string
		selective bool
		want      []string
		wantErr   bool
	}{
		{"", false, nil, false},
		{"no", false, nil, false},
		{"false", false, nil, false},
		{"yes", false, generated, false},
		{"true", false, generated, false},
		{"all", false, generated, false},
		{"one", true, []string{"one"}, false},
		{"one,two", true, generated, false},
		{"unknown", true, nil, true},
		{"one,one", true, nil, true},
		{"one", false, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := ParseRegenerate(tt.value, generated, tt.selective)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRegenerate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !bytes.Equal([]byte(joinFields(got)), []byte(joinFields(tt.want))) {
				t.Fatalf("ParseRegenerate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func joinFields(fields []string) string {
	var b bytes.Buffer
	for _, field := range fields {
		b.WriteString(field)
		b.WriteByte(0)
	}
	return b.String()
}

func TestValidationBoundaries(t *testing.T) {
	validName := bytes.Repeat([]byte{'a'}, MaxFieldNameLength)
	if err := ValidateFieldName(string(validName)); err != nil {
		t.Fatalf("valid max-length field: %v", err)
	}
	for _, name := range []string{"", string(append(validName, 'a')), "bad/name", "white space"} {
		if err := ValidateFieldName(name); err == nil {
			t.Fatalf("ValidateFieldName(%q) succeeded", name)
		}
	}
	for _, encoding := range []string{"base64", "base64url", "base32", "hex", "raw"} {
		if err := ValidateEncoding(encoding); err != nil {
			t.Fatalf("ValidateEncoding(%q): %v", encoding, err)
		}
	}
	if err := ValidateEncoding("BASE64"); err == nil {
		t.Fatal("unknown encoding succeeded")
	}
	if err := ValidateManagedFields([]string{"same"}, map[string][]byte{"same": nil}); err == nil {
		t.Fatal("generated/literal collision succeeded")
	}
	generated := make([]string, MaxGeneratedFields)
	for i := range generated {
		generated[i] = fmt.Sprintf("generated-%d", i)
	}
	literal := make(map[string][]byte, MaxManagedKeys-MaxGeneratedFields)
	for i := 0; i < MaxManagedKeys-MaxGeneratedFields; i++ {
		literal[fmt.Sprintf("literal-%d", i)] = nil
	}
	if err := ValidateManagedFields(generated, literal); err != nil {
		t.Fatalf("maximum managed fields: %v", err)
	}
	if err := ValidateManagedFields(append(generated, "generated-overflow"), nil); err == nil {
		t.Fatal("65 generated fields succeeded")
	}
	literal["literal-overflow"] = nil
	if err := ValidateManagedFields(generated, literal); err == nil {
		t.Fatal("257 managed fields succeeded")
	}
}

func TestBasicAuthValidation(t *testing.T) {
	for _, username := range []string{"bad:name", "bad\rname", "bad\nname", "bad\x00name", ""} {
		if err := ValidateBasicAuthUsername(username); err == nil {
			t.Fatalf("ValidateBasicAuthUsername(%q) succeeded", username)
		}
	}
	if _, err := BasicAuthProjectedValueLengths(&BasicAuthConstraints{Username: "user", Encoding: "raw", Length: "72"}); err != nil {
		t.Fatalf("72-byte password: %v", err)
	}
	if _, err := BasicAuthProjectedValueLengths(&BasicAuthConstraints{Username: "user", Encoding: "raw", Length: "73"}); err == nil {
		t.Fatal("73-byte bcrypt password succeeded")
	}
	if _, err := BasicAuthProjectedValueLengths(&BasicAuthConstraints{Username: "user", Encoding: "base64", Length: "54B"}); err != nil {
		t.Fatalf("72-byte encoded password: %v", err)
	}
	if _, err := BasicAuthProjectedValueLengths(&BasicAuthConstraints{Username: "user", Encoding: "base64", Length: "55B"}); err == nil {
		t.Fatal("76-byte encoded bcrypt password succeeded")
	}
}

func TestSSHConfigurationMatrix(t *testing.T) {
	valid := [][2]string{{"rsa", "2048"}, {"rsa", "3072"}, {"rsa", "4096"}, {"ecdsa", "256"}, {"ecdsa", "384"}, {"ecdsa", "521"}, {"ed25519", "65537"}}
	for _, pair := range valid {
		if _, _, err := ValidateSSHConfiguration(pair[0], pair[1]); err != nil {
			t.Fatalf("ValidateSSHConfiguration(%q, %q): %v", pair[0], pair[1], err)
		}
	}
	invalid := [][2]string{{"rsa", "1024"}, {"rsa", "4097"}, {"rsa", "256B"}, {"ecdsa", "512"}, {"unknown", "2048"}}
	for _, pair := range invalid {
		if _, _, err := ValidateSSHConfiguration(pair[0], pair[1]); err == nil {
			t.Fatalf("ValidateSSHConfiguration(%q, %q) succeeded", pair[0], pair[1])
		}
	}
}

func TestCheckAndRegenPublicKeyRepairsMismatch(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM, err := pemBytesForPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{"public": []byte("mismatch")}
	if err := CheckAndRegenPublicKey(data, data["public"], privatePEM, "public"); err != nil {
		t.Fatal(err)
	}
	want, err := SSHPublicKeyForPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data["public"], want) {
		t.Fatal("public key mismatch was not repaired")
	}
}

func TestInvalidRegenerateAnnotationIsRetained(t *testing.T) {
	instance := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationSecretAutoGenerate: "value",
		AnnotationSecretRegenerate:   "unknown",
	}}, Data: map[string][]byte{}}
	if _, err := (StringGenerator{}).generateData(instance); !IsValidationError(err) {
		t.Fatalf("generateData() error = %v, want ValidationError", err)
	}
	if instance.Annotations[AnnotationSecretRegenerate] != "unknown" {
		t.Fatal("invalid regenerate annotation was removed")
	}
	if len(instance.Data) != 0 {
		t.Fatal("invalid regenerate annotation generated data")
	}
}

func TestProjectedSecretSizeBoundary(t *testing.T) {
	projected := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"padding": ""}}}
	for n := MaxProjectedSecretSize; n >= 0; n-- {
		projected.Annotations["padding"] = strings.Repeat("a", n)
		if ValidateProjectedSecretSize(projected) == nil {
			break
		}
	}
	if err := ValidateProjectedSecretSize(projected); err != nil {
		t.Fatalf("boundary Secret rejected: %v", err)
	}
	projected.Annotations["padding"] += "a"
	if err := ValidateProjectedSecretSize(projected); err == nil {
		t.Fatal("oversized Secret succeeded")
	}
}

func FuzzParseByteLength(f *testing.F) {
	for _, seed := range []string{"", "1", "65536B", "-1", "999999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		length, _, err := ParseByteLength(40, input)
		if err == nil && (length < MinSecretLength || length > MaxSecretLength) {
			t.Fatalf("accepted out-of-range length %d", length)
		}
	})
}

func FuzzValidateEncoding(f *testing.F) {
	for _, seed := range []string{"base64", "base64url", "base32", "hex", "raw", "BASE64", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		err := ValidateEncoding(input)
		valid := input == "base64" || input == "base64url" || input == "base32" || input == "hex" || input == "raw"
		if (err == nil) != valid {
			t.Fatalf("ValidateEncoding(%q) error = %v", input, err)
		}
	})
}

func FuzzParseRegenerate(f *testing.F) {
	for _, seed := range []string{"", "no", "false", "yes", "true", "one", "one,two", "unknown", "one,one"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		fields, err := ParseRegenerate(input, []string{"one", "two"}, true)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, field := range fields {
			if field != "one" && field != "two" {
				t.Fatalf("unexpected field %q", field)
			}
			if seen[field] {
				t.Fatalf("duplicate field %q", field)
			}
			seen[field] = true
		}
	})
}

func FuzzPrivateKeyPEM(f *testing.F) {
	f.Add([]byte("not a PEM key"))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > MaxPrivateKeyPEMBytes+1 {
			input = input[:MaxPrivateKeyPEMBytes+1]
		}
		_, _ = rawPrivateKeyFromPEM(input)
	})
}
