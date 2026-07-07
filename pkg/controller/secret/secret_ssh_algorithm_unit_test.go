package secret

import (
	"bytes"
	"testing"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
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
