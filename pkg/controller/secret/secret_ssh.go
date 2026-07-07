package secret

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	SecretFieldPublicKey  = "ssh-publickey"
	SecretFieldPrivateKey = "ssh-privatekey"

	SSHKeyAlgorithmRSA     = "rsa"
	SSHKeyAlgorithmECDSA   = "ecdsa"
	SSHKeyAlgorithmED25519 = "ed25519"
)

type SSHKeypairGenerator struct {
	log logr.Logger
}

func (sg SSHKeypairGenerator) generateData(instance *corev1.Secret) (reconcile.Result, error) {
	regenerate := instance.Annotations[AnnotationSecretRegenerate] != ""

	if regenerate {
		delete(instance.Annotations, AnnotationSecretRegenerate)
	}

	length, err := GetLengthFromAnnotation(SSHKeyLength(), instance.Annotations)
	if err != nil {
		return reconcile.Result{}, err
	}

	algorithm := instance.Annotations[AnnotationSSHKeyAlgorithm]
	if algorithm == "" {
		algorithm = SSHKeyAlgorithm()
	}
	if _, ok := instance.Annotations[AnnotationSecretLength]; !ok {
		switch algorithm {
		case SSHKeyAlgorithmECDSA:
			length = "256"
		case SSHKeyAlgorithmED25519:
			length = ""
		}
	}
	if instance.Data == nil {
		instance.Data = make(map[string][]byte)
	}
	err = GenerateSSHKeypairDataWithAlgorithm(sg.log, algorithm, length, regenerate, instance.Data)
	if err != nil {
		return reconcile.Result{RequeueAfter: time.Second * 30}, err
	}

	return reconcile.Result{}, nil
}

// generates ssh private and public key of given length
// and writes the result to data. The public key is in authorized-keys format,
// the private key is PEM encoded
func GenerateSSHKeypairData(logger logr.Logger, length string, regenerate bool, data map[string][]byte) error {
	return GenerateSSHKeypairDataWithAlgorithm(logger, SSHKeyAlgorithmRSA, length, regenerate, data)
}

func GenerateSSHKeypairDataWithAlgorithm(logger logr.Logger, algorithm, length string, regenerate bool, data map[string][]byte) error {
	privateKey := data[SecretFieldPrivateKey]
	publicKey := data[SecretFieldPublicKey]

	if len(privateKey) > 0 && !regenerate {
		return CheckAndRegenPublicKey(data, publicKey, privateKey)
	}

	key, err := generateNewPrivateKey(algorithm, length, logger)
	if err != nil {
		return err
	}

	return generateKeysHelper(key, data)
}

// generateNewPrivateKey parses the given length and generates a matching private key
func generateNewPrivateKey(algorithm, length string, logger logr.Logger) (interface{}, error) {
	if algorithm == "" {
		algorithm = SSHKeyAlgorithm()
	}

	switch algorithm {
	case SSHKeyAlgorithmRSA:
		parsedLen, isByte, err := ParseByteLength(SSHKeyLength(), length)
		if err != nil {
			logger.Error(err, "could not parse length for new rsa key")

			return nil, err
		}
		if isByte {
			parsedLen *= 8
		}
		return rsa.GenerateKey(rand.Reader, parsedLen)
	case SSHKeyAlgorithmECDSA:
		parsedLen, isByte, err := ParseByteLength(256, length)
		if err != nil {
			logger.Error(err, "could not parse length for new ecdsa key")

			return nil, err
		}
		if isByte {
			parsedLen *= 8
		}
		curve, err := ecdsaCurve(parsedLen)
		if err != nil {
			return nil, err
		}
		return ecdsa.GenerateKey(curve, rand.Reader)
	case SSHKeyAlgorithmED25519:
		_, key, err := ed25519.GenerateKey(rand.Reader)
		return key, err
	default:
		return nil, fmt.Errorf("unsupported ssh key algorithm %q", algorithm)
	}
}

// generateKeysHelper generates the public key from the given private key and stores the result in data
func generateKeysHelper(key interface{}, data map[string][]byte) error {
	privateKeyBytes, err := pemBytesForPrivateKey(key)
	if err != nil {
		return err
	}

	var publicKeyBytes []byte
	publicKeyBytes, err = SSHPublicKeyForPrivateKey(key)
	if err != nil {
		return err
	}

	data[SecretFieldPublicKey] = publicKeyBytes
	data[SecretFieldPrivateKey] = privateKeyBytes

	return nil
}

func ecdsaCurve(length int) (elliptic.Curve, error) {
	switch length {
	case 256:
		return elliptic.P256(), nil
	case 384:
		return elliptic.P384(), nil
	case 521:
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported ecdsa key length %d", length)
	}
}

func pemBytesForPrivateKey(key interface{}) ([]byte, error) {
	privateKeyBytes := &bytes.Buffer{}

	var block *pem.Block
	switch key := key.(type) {
	case *rsa.PrivateKey:
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	case *ecdsa.PrivateKey:
		keyBytes, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		block = &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}
	case ed25519.PrivateKey:
		keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}
	default:
		return nil, fmt.Errorf("unsupported private key type %T", key)
	}

	err := pem.Encode(privateKeyBytes, block)
	if err != nil {
		return nil, err
	}

	return privateKeyBytes.Bytes(), nil
}

func rawPrivateKeyFromPEM(pemKey []byte) (interface{}, error) {
	return ssh.ParseRawPrivateKey(pemKey)
}

func PrivateKeyFromPEM(pemKey []byte) (*rsa.PrivateKey, error) {
	key, err := rawPrivateKeyFromPEM(pemKey)
	if err != nil {
		return nil, err
	}

	privateKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not rsa")
	}
	return privateKey, nil
}

func SSHPublicKeyForPrivateKey(privateKey interface{}) ([]byte, error) {
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return nil, err
	}

	return ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
}

// CheckAndRegenPublicKey checks if the specified public key has length > 0 and regenerates it from the given private key
// otherwise. The result is written into data
func CheckAndRegenPublicKey(data map[string][]byte, publicKey, privateKey []byte) error {
	if len(publicKey) > 0 {
		return nil
	}

	// restore public key if private key exists
	key, err := rawPrivateKeyFromPEM(privateKey)
	if err != nil {
		return err
	}
	publicKey, err = SSHPublicKeyForPrivateKey(key)
	if err != nil {
		return err
	}
	data[SecretFieldPublicKey] = publicKey

	return nil
}
