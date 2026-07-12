package secret

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"golang.org/x/crypto/bcrypt"
)

// Ingress basic auth secret field
const FieldBasicAuthIngress = "auth"
const FieldBasicAuthUsername = "username"
const FieldBasicAuthPassword = "password"

type BasicAuthGenerator struct {
	log logr.Logger
}

type BasicAuthConstraints struct {
	Username string
	Encoding string
	Length   string
}

func (bg BasicAuthGenerator) generateData(instance *corev1.Secret) (reconcile.Result, error) {
	existingAuth := string(instance.Data[FieldBasicAuthIngress])
	regenerate := false
	regenerateRequested := false
	if value, ok := instance.Annotations[AnnotationSecretRegenerate]; ok {
		regenerateRequested = true
		fields, err := ParseRegenerate(value, []string{FieldBasicAuthIngress, FieldBasicAuthUsername, FieldBasicAuthPassword}, false)
		if err != nil {
			return reconcile.Result{}, err
		}
		regenerate = len(fields) > 0
	}

	username := instance.Annotations[AnnotationBasicAuthUsername]

	length, err := GetLengthFromAnnotation(DefaultLength(), instance.Annotations)
	if err != nil {
		return reconcile.Result{}, err
	}

	var encoding string
	encoding, err = getEncodingFromAnnotation(DefaultEncoding(), instance.Annotations)
	if err != nil {
		return reconcile.Result{}, err
	}

	constraints := &BasicAuthConstraints{Encoding: encoding, Length: length, Username: username}
	projected, err := BasicAuthProjectedValueLengths(constraints)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(existingAuth) > 0 && !regenerate {
		if err := ValidateProjectedSecretSize(instance); err != nil {
			return reconcile.Result{}, err
		}
		if regenerateRequested {
			delete(instance.Annotations, AnnotationSecretRegenerate)
		}
		return reconcile.Result{}, nil
	} else {
		if err := ValidateProjectedDataSize(instance, projected); err != nil {
			return reconcile.Result{}, err
		}
	}
	err = GenerateBasicAuthData(bg.log, constraints, instance.Data)
	if err != nil {
		return reconcile.Result{}, err
	}
	if regenerateRequested {
		delete(instance.Annotations, AnnotationSecretRegenerate)
	}

	return reconcile.Result{}, nil
}

func GenerateBasicAuthData(logger logr.Logger, cons *BasicAuthConstraints, data map[string][]byte) error {
	if _, err := BasicAuthProjectedValueLengths(cons); err != nil {
		return err
	}
	parsedLen, isByteLength, _ := ParseByteLength(DefaultLength(), cons.Length)

	password, err := GenerateRandomString(parsedLen, cons.Encoding, isByteLength)
	if err != nil {
		return err
	}

	passwordHash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	data[FieldBasicAuthIngress] = append([]byte(cons.Username+":"), passwordHash...)
	data[FieldBasicAuthUsername] = []byte(cons.Username)
	data[FieldBasicAuthPassword] = password

	return ValidateBasicAuthCredential(data)
}

func BasicAuthProjectedValueLengths(cons *BasicAuthConstraints) (map[string]int, error) {
	if cons.Username == "" {
		cons.Username = "admin"
	}
	if err := ValidateBasicAuthUsername(cons.Username); err != nil {
		return nil, err
	}
	if err := ValidateEncoding(cons.Encoding); err != nil {
		return nil, err
	}
	parsedLen, isByteLength, err := ParseByteLength(DefaultLength(), cons.Length)
	if err != nil {
		return nil, err
	}
	passwordBytes, err := EncodedOutputLength(parsedLen, cons.Encoding, isByteLength)
	if err != nil {
		return nil, err
	}
	if passwordBytes > 72 {
		return nil, validationError("password", "encoded bcrypt input is %d bytes; maximum is 72", passwordBytes)
	}
	return map[string]int{
		FieldBasicAuthIngress:  len(cons.Username) + 1 + 60,
		FieldBasicAuthUsername: len(cons.Username),
		FieldBasicAuthPassword: passwordBytes,
	}, nil
}

func ValidateBasicAuthUsername(username string) error {
	if !utf8.ValidString(username) {
		return validationError("username", "must be valid UTF-8")
	}
	if count := utf8.RuneCountInString(username); count < 1 || count > 255 {
		return validationError("username", "length must be between 1 and 255 characters")
	}
	if strings.ContainsAny(username, ":\r\n\x00") {
		return validationError("username", "must not contain colon, CR, LF, or NUL")
	}
	return nil
}

func ValidateBasicAuthLiteralData(data map[string][]byte) error {
	for _, field := range []string{FieldBasicAuthIngress, FieldBasicAuthUsername, FieldBasicAuthPassword} {
		if _, ok := data[field]; ok {
			return validationError("data", "field %q is reserved for BasicAuth", field)
		}
	}
	return nil
}

func ValidateBasicAuthCredential(data map[string][]byte) error {
	username := data[FieldBasicAuthUsername]
	password := data[FieldBasicAuthPassword]
	auth := data[FieldBasicAuthIngress]
	prefix := append(append([]byte(nil), username...), ':')
	if !bytes.HasPrefix(auth, prefix) {
		return fmt.Errorf("generated basic auth username does not match auth field")
	}
	if err := bcrypt.CompareHashAndPassword(auth[len(prefix):], password); err != nil {
		return fmt.Errorf("generated basic auth password does not match bcrypt hash: %w", err)
	}
	return nil
}

// EncodedOutputLength returns the generated value size without allocating it.
func EncodedOutputLength(length int, encoding string, lenBytes bool) (int, error) {
	if err := ValidateLength(length); err != nil {
		return 0, err
	}
	if err := ValidateEncoding(encoding); err != nil {
		return 0, err
	}
	if !lenBytes || encoding == "raw" {
		return length, nil
	}
	switch encoding {
	case "base64", "base64url":
		return base64.StdEncoding.EncodedLen(length), nil
	case "base32":
		return base32.StdEncoding.EncodedLen(length), nil
	case "hex":
		return hex.EncodedLen(length), nil
	default:
		return length, nil
	}
}
