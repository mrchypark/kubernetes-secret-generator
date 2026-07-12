package secret

import (
	"bytes"
	"crypto/ecdsa"
	"testing"

	"github.com/go-logr/logr"
	"github.com/spf13/viper"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGenerateSSHKeypairDataWithAlgorithm(t *testing.T) {
	var log logr.Logger
	tests := []struct {
		name      string
		algorithm string
		length    string
		wantType  string
	}{
		{"default rsa", "", "2048", "ssh-rsa"},
		{"rsa", "rsa", "2048", "ssh-rsa"},
		{"ecdsa", "ecdsa", "256", "ecdsa-sha2-nistp256"},
		{"ed25519", "ed25519", "", "ssh-ed25519"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := map[string][]byte{}
			if err := GenerateSSHKeypairDataWithAlgorithm(log, tt.algorithm, tt.length, DefaultSecretFieldPrivateKey, DefaultSecretFieldPublicKey, true, data); err != nil {
				t.Fatalf("generate keypair: %v", err)
			}

			signer, err := ssh.ParsePrivateKey(data[SecretFieldPrivateKey])
			if err != nil {
				t.Fatalf("parse private key: %v", err)
			}
			if signer.PublicKey().Type() != tt.wantType {
				t.Fatalf("public key type = %s, want %s", signer.PublicKey().Type(), tt.wantType)
			}
			if !bytes.Equal(data[SecretFieldPublicKey], ssh.MarshalAuthorizedKey(signer.PublicKey())) {
				t.Fatalf("public key does not match private key")
			}
			algorithm, strength, err := ValidateSSHConfiguration(tt.algorithm, tt.length)
			if err != nil {
				t.Fatalf("validate configuration: %v", err)
			}
			if _, err := ValidateSSHPrivateKey(data[SecretFieldPrivateKey], algorithm, strength); err != nil {
				t.Fatalf("validate generated private key: %v", err)
			}
		})
	}
}

func TestGenerateSSHKeypairDataWithAlgorithmRejectsUnknownAlgorithm(t *testing.T) {
	var log logr.Logger
	err := GenerateSSHKeypairDataWithAlgorithm(log, "ecdsa-sk", "", DefaultSecretFieldPrivateKey, DefaultSecretFieldPublicKey, true, map[string][]byte{})
	if err == nil {
		t.Fatal("expected unsupported algorithm error")
	}
}

func TestGenerateSSHKeypairDataWithAlgorithmRejectsInvalidStrength(t *testing.T) {
	var log logr.Logger
	tests := []struct {
		name      string
		algorithm string
		length    string
	}{
		{"weak rsa", "rsa", "1024"},
		{"rsa byte suffix", "rsa", "256B"},
		{"unsupported rsa", "rsa", "4097"},
		{"ecdsa byte suffix", "ecdsa", "32b"},
		{"unsupported ecdsa", "ecdsa", "512"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := map[string][]byte{}
			if err := GenerateSSHKeypairDataWithAlgorithm(log, tt.algorithm, tt.length, DefaultSecretFieldPrivateKey, DefaultSecretFieldPublicKey, true, data); err == nil {
				t.Fatal("expected invalid strength error")
			}
			if len(data) != 0 {
				t.Fatal("invalid strength mutated data")
			}
		})
	}
}

func TestSSHKeypairGeneratorDefaultsECDSAToP256WithNilData(t *testing.T) {
	instance := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationSSHKeyAlgorithm: SSHKeyAlgorithmECDSA,
			},
		},
	}

	_, err := SSHKeypairGenerator{}.generateData(instance)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}

	key, err := rawPrivateKeyFromPEM(instance.Data[SecretFieldPrivateKey])
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	privateKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("key type = %T, want *ecdsa.PrivateKey", key)
	}
	if privateKey.Curve.Params().BitSize != 256 {
		t.Fatalf("ecdsa curve size = %d bits, want 256", privateKey.Curve.Params().BitSize)
	}
}

func TestSSHKeypairGeneratorNormalizesAlgorithmBeforeDefaultLength(t *testing.T) {
	instance := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationSSHKeyAlgorithm: " ECDSA ",
			},
		},
	}

	_, err := SSHKeypairGenerator{}.generateData(instance)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}

	key, err := rawPrivateKeyFromPEM(instance.Data[SecretFieldPrivateKey])
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	privateKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("key type = %T, want *ecdsa.PrivateKey", key)
	}
	if privateKey.Curve.Params().BitSize != 256 {
		t.Fatalf("ecdsa curve size = %d bits, want 256", privateKey.Curve.Params().BitSize)
	}
}

func TestSSHKeypairGeneratorDefaultsGlobalECDSAToP256(t *testing.T) {
	viper.Set("ssh-key-algorithm", SSHKeyAlgorithmECDSA)
	t.Cleanup(func() {
		viper.Set("ssh-key-algorithm", SSHKeyAlgorithmRSA)
	})

	instance := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{},
		},
		Data: map[string][]byte{},
	}

	_, err := SSHKeypairGenerator{}.generateData(instance)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}

	key, err := rawPrivateKeyFromPEM(instance.Data[SecretFieldPrivateKey])
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	privateKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("key type = %T, want *ecdsa.PrivateKey", key)
	}
	if privateKey.Curve.Params().BitSize != 256 {
		t.Fatalf("ecdsa curve size = %d bits, want 256", privateKey.Curve.Params().BitSize)
	}
}
