package secret

import (
	"bytes"
	"fmt"

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
	regenerate := instance.Annotations[AnnotationSecretRegenerate] != ""

	if len(existingAuth) > 0 && !regenerate {
		return reconcile.Result{}, nil
	}

	delete(instance.Annotations, AnnotationSecretRegenerate)

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

	err = GenerateBasicAuthData(bg.log, &BasicAuthConstraints{Encoding: encoding, Length: length, Username: username}, instance.Data)
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func GenerateBasicAuthData(logger logr.Logger, cons *BasicAuthConstraints, data map[string][]byte) error {
	if cons.Username == "" {
		cons.Username = "admin"
	}

	parsedLen, isByteLength, err := ParseByteLength(DefaultLength(), cons.Length)
	if err != nil {
		logger.Error(err, "could not parse length for new random string")

		return err
	}

	var password []byte
	password, err = GenerateRandomString(parsedLen, cons.Encoding, isByteLength)
	if err != nil {
		logger.Error(err, "could not generate random string")

		return err
	}

	var passwordHash []byte
	passwordHash, err = bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		logger.Error(err, "could not hash random string")

		return err
	}

	data[FieldBasicAuthIngress] = append([]byte(cons.Username+":"), passwordHash...)
	data[FieldBasicAuthUsername] = []byte(cons.Username)
	data[FieldBasicAuthPassword] = password

	return nil
}

// RepairBasicAuthData fills only missing generated fields. Existing nonempty
// values are never replaced; inconsistent or non-derivable partial state is
// rejected without mutating data.
func RepairBasicAuthData(logger logr.Logger, cons *BasicAuthConstraints, data map[string][]byte) error {
	auth := data[FieldBasicAuthIngress]
	username := data[FieldBasicAuthUsername]
	password := data[FieldBasicAuthPassword]

	if len(auth) > 0 && len(username) > 0 && len(password) > 0 {
		return nil
	}
	if len(auth) == 0 && len(username) == 0 && len(password) == 0 {
		return GenerateBasicAuthData(logger, cons, data)
	}
	if len(password) == 0 {
		return fmt.Errorf("cannot restore missing basic-auth password from its bcrypt hash")
	}

	pending := make(map[string][]byte)
	if len(username) == 0 {
		parsedUsername, hash, ok := bytes.Cut(auth, []byte(":"))
		if !ok || len(parsedUsername) == 0 || len(hash) == 0 {
			return fmt.Errorf("cannot restore basic-auth username from malformed auth field")
		}
		if err := bcrypt.CompareHashAndPassword(hash, password); err != nil {
			return fmt.Errorf("cannot restore basic-auth username from inconsistent auth and password: %w", err)
		}
		pending[FieldBasicAuthUsername] = append([]byte(nil), parsedUsername...)
		username = parsedUsername
	}

	if len(auth) == 0 {
		hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash existing basic-auth password: %w", err)
		}
		pending[FieldBasicAuthIngress] = append(append([]byte(nil), username...), append([]byte(":"), hash...)...)
	}

	for key, value := range pending {
		data[key] = value
	}
	return nil
}
