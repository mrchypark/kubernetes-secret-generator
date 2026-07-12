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
	"strings"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	DefaultSecretFieldPublicKey  = "ssh-publickey"
	DefaultSecretFieldPrivateKey = "ssh-privatekey"

	SecretFieldPublicKey  = DefaultSecretFieldPublicKey
	SecretFieldPrivateKey = DefaultSecretFieldPrivateKey

	SSHKeyAlgorithmRSA     = "rsa"
	SSHKeyAlgorithmECDSA   = "ecdsa"
	SSHKeyAlgorithmED25519 = "ed25519"
)

type SSHKeypairGenerator struct {
	log logr.Logger
}

func (sg SSHKeypairGenerator) generateData(instance *corev1.Secret) (reconcile.Result, error) {
	regenerate := false
	regenerateRequested := false
	if value, ok := instance.Annotations[AnnotationSecretRegenerate]; ok {
		regenerateRequested = true
		fields, err := ParseRegenerate(value, []string{DefaultSecretFieldPrivateKey, DefaultSecretFieldPublicKey}, false)
		if err != nil {
			return reconcile.Result{}, err
		}
		regenerate = len(fields) > 0
	}

	length, err := GetLengthFromAnnotation(SSHKeyLength(), instance.Annotations)
	if err != nil {
		return reconcile.Result{}, err
	}

	algorithm := normalizeSSHKeyAlgorithm(instance.Annotations[AnnotationSSHKeyAlgorithm])
	if algorithm == "" {
		algorithm = normalizeSSHKeyAlgorithm(SSHKeyAlgorithm())
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

	privateKeyField, err := GetPrivateKeyFieldFromAnnotation(DefaultSecretFieldPrivateKey, instance.Annotations)
	if err != nil {
		return reconcile.Result{}, err
	}

	publicKeyField, err := GetPublicKeyFieldFromAnnotation(DefaultSecretFieldPublicKey, instance.Annotations)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := ValidateSSHFields(privateKeyField, publicKeyField, nil); err != nil {
		return reconcile.Result{}, err
	}
	algorithm, expectedLength, err := ValidateSSHConfiguration(algorithm, length)
	if err != nil {
		return reconcile.Result{}, err
	}
	projected, err := SSHProjectedValueLengths(algorithm, expectedLength, privateKeyField, publicKeyField, regenerate, instance.Data)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := ValidateProjectedDataSize(instance, projected); err != nil {
		return reconcile.Result{}, err
	}

	err = GenerateSSHKeypairDataWithAlgorithm(sg.log, algorithm, length, privateKeyField, publicKeyField, regenerate, instance.Data)
	if err != nil {
		return reconcile.Result{}, err
	}
	if regenerateRequested {
		delete(instance.Annotations, AnnotationSecretRegenerate)
	}

	return reconcile.Result{}, nil
}

// generates ssh private and public key of given length
// and writes the result to data. The public key is in authorized-keys format,
// the private key is PEM encoded
func GenerateSSHKeypairData(logger logr.Logger, length string, privateKeyField string, publicKeyField string, regenerate bool, data map[string][]byte) error {
	return GenerateSSHKeypairDataWithAlgorithm(logger, SSHKeyAlgorithmRSA, length, privateKeyField, publicKeyField, regenerate, data)
}

func GenerateSSHKeypairDataWithAlgorithm(logger logr.Logger, algorithm, length string, privateKeyField string, publicKeyField string, regenerate bool, data map[string][]byte) error {
	algorithm, expectedLength, err := ValidateSSHConfiguration(algorithm, length)
	if err != nil {
		return err
	}
	if err := ValidateSSHFields(privateKeyField, publicKeyField, nil); err != nil {
		return err
	}
	privateKey := data[privateKeyField]
	publicKey := data[publicKeyField]

	if len(privateKey) > 0 && !regenerate {
		key, err := ValidateSSHPrivateKey(privateKey, algorithm, expectedLength)
		if err != nil {
			return err
		}
		return checkAndRepairPublicKey(data, publicKey, key, publicKeyField)
	}

	key, err := generateNewPrivateKey(algorithm, length, logger)
	if err != nil {
		return err
	}

	return generateKeysHelper(key, privateKeyField, publicKeyField, data)
}

func normalizeSSHKeyAlgorithm(algorithm string) string {
	return strings.ToLower(strings.TrimSpace(algorithm))
}

// ValidateSSHConfiguration normalizes and validates the complete algorithm /
// strength matrix before any key generation is attempted.
func ValidateSSHConfiguration(algorithm, length string) (string, int, error) {
	algorithm = normalizeSSHKeyAlgorithm(algorithm)
	if algorithm == "" {
		algorithm = normalizeSSHKeyAlgorithm(SSHKeyAlgorithm())
	}
	if algorithm == SSHKeyAlgorithmED25519 {
		return algorithm, 0, nil
	}
	fallback := SSHKeyLength()
	if algorithm == SSHKeyAlgorithmECDSA {
		fallback = 256
	}
	parsed, isByte, err := ParseByteLength(fallback, length)
	if err != nil {
		return "", 0, err
	}
	if isByte {
		return "", 0, validationError("ssh key length", "B suffix is not supported; specify strength in bits")
	}
	switch algorithm {
	case SSHKeyAlgorithmRSA:
		if parsed != 2048 && parsed != 3072 && parsed != 4096 {
			return "", 0, validationError("ssh key length", "RSA strength must be 2048, 3072, or 4096 bits")
		}
	case SSHKeyAlgorithmECDSA:
		if parsed != 256 && parsed != 384 && parsed != 521 {
			return "", 0, validationError("ssh key length", "ECDSA strength must be 256, 384, or 521 bits")
		}
	default:
		return "", 0, validationError("ssh key algorithm", "unsupported value %q", algorithm)
	}
	return algorithm, parsed, nil
}

func ValidateSSHFields(privateKeyField, publicKeyField string, literal map[string][]byte) error {
	if err := ValidateFieldName(privateKeyField); err != nil {
		return err
	}
	if err := ValidateFieldName(publicKeyField); err != nil {
		return err
	}
	if privateKeyField == publicKeyField {
		return validationError("ssh key fields", "private and public key fields must differ")
	}
	for _, field := range []string{privateKeyField, publicKeyField} {
		if _, ok := literal[field]; ok {
			return validationError("data", "field %q collides with an SSH key field", field)
		}
	}
	return nil
}

func SSHProjectedValueLengths(algorithm string, expectedLength int, privateKeyField, publicKeyField string, regenerate bool, data map[string][]byte) (map[string]int, error) {
	if privateKey := data[privateKeyField]; len(privateKey) > 0 && !regenerate {
		key, err := ValidateSSHPrivateKey(privateKey, algorithm, expectedLength)
		if err != nil {
			return nil, err
		}
		publicKey, err := SSHPublicKeyForPrivateKey(key)
		if err != nil {
			return nil, err
		}
		return map[string]int{privateKeyField: len(privateKey), publicKeyField: len(publicKey)}, nil
	}

	privateMax, publicMax := 512, 512
	switch algorithm {
	case SSHKeyAlgorithmRSA:
		privateMax, publicMax = expectedLength, expectedLength/4
	case SSHKeyAlgorithmECDSA:
		privateMax, publicMax = expectedLength*2, expectedLength*2
	}
	return map[string]int{privateKeyField: privateMax, publicKeyField: publicMax}, nil
}

// generateNewPrivateKey parses the given length and generates a matching private key
func generateNewPrivateKey(algorithm, length string, logger logr.Logger) (interface{}, error) {
	_ = logger
	algorithm, parsedLen, err := ValidateSSHConfiguration(algorithm, length)
	if err != nil {
		return nil, err
	}

	switch algorithm {
	case SSHKeyAlgorithmRSA:
		return rsa.GenerateKey(rand.Reader, parsedLen)
	case SSHKeyAlgorithmECDSA:
		curve, err := ecdsaCurve(parsedLen)
		if err != nil {
			return nil, err
		}
		return ecdsa.GenerateKey(curve, rand.Reader)
	case SSHKeyAlgorithmED25519:
		_, key, err := ed25519.GenerateKey(rand.Reader)
		return key, err
	}
	return nil, validationError("ssh key algorithm", "unsupported value %q", algorithm)
}

// generateKeysHelper generates the public key from the given private key and stores the result in data
func generateKeysHelper(key interface{}, privateKeyField string, publicKeyField string, data map[string][]byte) error {
	privateKeyBytes, err := pemBytesForPrivateKey(key)
	if err != nil {
		return err
	}

	var publicKeyBytes []byte
	publicKeyBytes, err = SSHPublicKeyForPrivateKey(key)
	if err != nil {
		return err
	}

	data[publicKeyField] = publicKeyBytes
	data[privateKeyField] = privateKeyBytes

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
	if len(pemKey) == 0 || len(pemKey) > MaxPrivateKeyPEMBytes {
		return nil, validationError("private key", "PEM size must be between 1 and %d bytes", MaxPrivateKeyPEMBytes)
	}
	return ssh.ParseRawPrivateKey(pemKey)
}

// ValidateSSHPrivateKey validates supplied PEM type and strength against the
// desired matrix and returns the parsed key for public-key derivation.
func ValidateSSHPrivateKey(pemKey []byte, algorithm string, expectedLength int) (interface{}, error) {
	key, err := rawPrivateKeyFromPEM(pemKey)
	if err != nil {
		return nil, validationError("private key", "cannot parse PEM: %v", err)
	}
	switch algorithm {
	case SSHKeyAlgorithmRSA:
		privateKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, validationError("private key", "type %T does not match RSA", key)
		}
		if err := privateKey.Validate(); err != nil {
			return nil, validationError("private key", "invalid RSA key: %v", err)
		}
		bits := privateKey.N.BitLen()
		if (bits != 2048 && bits != 3072 && bits != 4096) || bits != expectedLength {
			return nil, validationError("private key", "RSA strength %d does not match required %d", bits, expectedLength)
		}
	case SSHKeyAlgorithmECDSA:
		privateKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, validationError("private key", "type %T does not match ECDSA", key)
		}
		bits := privateKey.Curve.Params().BitSize
		if (bits != 256 && bits != 384 && bits != 521) || bits != expectedLength {
			return nil, validationError("private key", "ECDSA strength %d does not match required %d", bits, expectedLength)
		}
	case SSHKeyAlgorithmED25519:
		switch key.(type) {
		case ed25519.PrivateKey, *ed25519.PrivateKey:
		default:
			return nil, validationError("private key", "type %T does not match Ed25519", key)
		}
	default:
		return nil, validationError("ssh key algorithm", "unsupported value %q", algorithm)
	}
	return key, nil
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
func CheckAndRegenPublicKey(data map[string][]byte, publicKey, privateKey []byte, publicKeyField string) error {
	key, err := rawPrivateKeyFromPEM(privateKey)
	if err != nil {
		return err
	}
	return checkAndRepairPublicKey(data, publicKey, key, publicKeyField)
}

func checkAndRepairPublicKey(data map[string][]byte, publicKey []byte, privateKey interface{}, publicKeyField string) error {
	want, err := SSHPublicKeyForPrivateKey(privateKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(publicKey, want) {
		data[publicKeyField] = want
	}

	return nil
}
