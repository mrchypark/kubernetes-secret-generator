package secret

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
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
		{"default rsa", "", "1024", "ssh-rsa"},
		{"rsa", "rsa", "1024", "ssh-rsa"},
		{"ecdsa", "ecdsa", "256", "ecdsa-sha2-nistp256"},
		{"ed25519", "ed25519", "", "ssh-ed25519"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := map[string][]byte{}
			if err := GenerateSSHKeypairDataWithAlgorithm(log, tt.algorithm, tt.length, true, data); err != nil {
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
		})
	}
}

func TestGenerateSSHKeypairDataWithAlgorithmRejectsUnknownAlgorithm(t *testing.T) {
	var log logr.Logger
	err := GenerateSSHKeypairDataWithAlgorithm(log, "ecdsa-sk", "", true, map[string][]byte{})
	if err == nil {
		t.Fatal("expected unsupported algorithm error")
	}
}

func TestGenerateSSHKeypairDataWithAlgorithmTreatsByteLengthAsBits(t *testing.T) {
	var log logr.Logger
	tests := []struct {
		name      string
		algorithm string
		length    string
		check     func(t *testing.T, key interface{})
	}{
		{
			name:      "rsa",
			algorithm: "rsa",
			length:    "128b",
			check: func(t *testing.T, key interface{}) {
				privateKey, ok := key.(*rsa.PrivateKey)
				if !ok {
					t.Fatalf("key type = %T, want *rsa.PrivateKey", key)
				}
				if privateKey.N.BitLen() != 1024 {
					t.Fatalf("rsa key length = %d bits, want 1024", privateKey.N.BitLen())
				}
			},
		},
		{
			name:      "ecdsa",
			algorithm: "ecdsa",
			length:    "32b",
			check: func(t *testing.T, key interface{}) {
				privateKey, ok := key.(*ecdsa.PrivateKey)
				if !ok {
					t.Fatalf("key type = %T, want *ecdsa.PrivateKey", key)
				}
				if privateKey.Curve.Params().BitSize != 256 {
					t.Fatalf("ecdsa curve size = %d bits, want 256", privateKey.Curve.Params().BitSize)
				}
			},
		},
		{
			name:      "ecdsa p521 byte length",
			algorithm: "ecdsa",
			length:    "66b",
			check: func(t *testing.T, key interface{}) {
				privateKey, ok := key.(*ecdsa.PrivateKey)
				if !ok {
					t.Fatalf("key type = %T, want *ecdsa.PrivateKey", key)
				}
				if privateKey.Curve.Params().BitSize != 521 {
					t.Fatalf("ecdsa curve size = %d bits, want 521", privateKey.Curve.Params().BitSize)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := map[string][]byte{}
			if err := GenerateSSHKeypairDataWithAlgorithm(log, tt.algorithm, tt.length, true, data); err != nil {
				t.Fatalf("generate keypair: %v", err)
			}

			key, err := rawPrivateKeyFromPEM(data[SecretFieldPrivateKey])
			if err != nil {
				t.Fatalf("parse private key: %v", err)
			}
			tt.check(t, key)
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
