package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
)

const annotationPrefix = "secret-generator.v1.mittwald.de/"

type input struct {
	Deployment json.RawMessage `json:"deployment"`
	Items      []object        `json:"items"`
}

type object struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metadata          `json:"metadata"`
	Spec       spec              `json:"spec"`
	Type       string            `json:"type"`
	Data       map[string]string `json:"data"`
	Immutable  bool              `json:"immutable"`
}

type metadata struct {
	Namespace       string            `json:"namespace"`
	Name            string            `json:"name"`
	UID             string            `json:"uid"`
	ResourceVersion string            `json:"resourceVersion"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	OwnerReferences []ownerReference  `json:"ownerReferences"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type spec struct {
	Type            string            `json:"type"`
	Data            map[string]string `json:"data"`
	Fields          []field           `json:"fields"`
	Username        string            `json:"username"`
	Length          string            `json:"length"`
	Encoding        string            `json:"encoding"`
	Algorithm       string            `json:"algorithm"`
	PrivateKey      string            `json:"privateKey"`
	PrivateKeyField string            `json:"privateKeyField"`
	PublicKeyField  string            `json:"publicKeyField"`
}

type field struct {
	FieldName string `json:"fieldName"`
	Length    string `json:"length"`
	Encoding  string `json:"encoding"`
}

type finding struct {
	Severity  string `json:"severity"`
	Code      string `json:"code"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Message   string `json:"message"`
	Field     string `json:"field,omitempty"`
}

type defaults struct {
	Length       int
	Encoding     string
	SSHAlgorithm string
	SSHLength    int
}

type result struct {
	Findings []finding      `json:"findings"`
	Snapshot []snapshotItem `json:"snapshot"`
	Defaults defaultsOutput `json:"defaults"`
}

type snapshotItem struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}

type defaultsOutput struct {
	Length       int    `json:"length"`
	Encoding     string `json:"encoding"`
	SSHAlgorithm string `json:"sshAlgorithm"`
	SSHLength    int    `json:"sshLength"`
}

func main() {
	if len(os.Args) != 1 {
		fatal(errors.New("usage: preflight-baseline"))
	}
	var in input
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<20)).Decode(&in); err != nil {
		fatal(err)
	}
	validated, err := validate(in)
	if err != nil {
		fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(validated); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "preflight baseline validation failed: %v\n", err)
	os.Exit(2)
}

func validate(in input) (result, error) {
	d, err := deploymentDefaults(in.Deployment)
	if err != nil {
		return result{}, err
	}
	secrets := make(map[string]object)
	for _, item := range in.Items {
		if item.Kind == "Secret" {
			secrets[key(item)] = item
		}
	}
	out := make([]finding, 0)
	snapshot := make([]snapshotItem, 0, len(in.Items))
	var deployment struct {
		Kind     string   `json:"kind"`
		Metadata metadata `json:"metadata"`
	}
	if err := json.Unmarshal(in.Deployment, &deployment); err != nil {
		return result{}, err
	}
	snapshot = append(snapshot, snapshotItem{Kind: defaultString(deployment.Kind, "Deployment"), Namespace: deployment.Metadata.Namespace, Name: deployment.Metadata.Name, UID: deployment.Metadata.UID, ResourceVersion: deployment.Metadata.ResourceVersion})
	for _, item := range in.Items {
		if item.Kind == "Secret" || item.Kind == "StringSecret" || item.Kind == "BasicAuth" || item.Kind == "SSHKeyPair" {
			snapshot = append(snapshot, snapshotItem{Kind: item.Kind, Namespace: item.Metadata.Namespace, Name: item.Metadata.Name, UID: item.Metadata.UID, ResourceVersion: item.Metadata.ResourceVersion})
		}
		switch item.Kind {
		case "StringSecret", "BasicAuth", "SSHKeyPair":
			if item.Kind == "SSHKeyPair" && item.Spec.PrivateKey != "" {
				algorithm := defaultString(strings.ToLower(item.Spec.Algorithm), d.SSHAlgorithm)
				length := sshLength(algorithm, item.Spec.Length, d.SSHLength)
				if _, err := parseSSHPrivate([]byte(item.Spec.PrivateKey), algorithm, length); err != nil {
					out = append(out, invalid(item, "spec.privateKey", "supplied SSH private key is malformed or does not match the effective algorithm and strength"))
				}
			}
			secret, ok := secrets[key(item)]
			if !ok || !exactOwner(secret, item) {
				continue
			}
			out = append(out, validateCR(item, secret, d)...)
		case "Secret":
			if annotationManaged(item) {
				out = append(out, validateAnnotation(item, d)...)
			}
		}
	}
	sort.Slice(snapshot, func(i, j int) bool {
		a, b := snapshot[i], snapshot[j]
		return a.Kind+"\x00"+a.Namespace+"\x00"+a.Name < b.Kind+"\x00"+b.Namespace+"\x00"+b.Name
	})
	return result{Findings: out, Snapshot: snapshot, Defaults: defaultsOutput{Length: d.Length, Encoding: d.Encoding, SSHAlgorithm: d.SSHAlgorithm, SSHLength: d.SSHLength}}, nil
}

func key(o object) string { return o.Metadata.Namespace + "\x00" + o.Metadata.Name }

func exactOwner(secret, cr object) bool {
	if len(secret.Metadata.OwnerReferences) != 1 {
		return false
	}
	ref := secret.Metadata.OwnerReferences[0]
	return ref.Controller && ref.APIVersion == cr.APIVersion && ref.Kind == cr.Kind && ref.Name == cr.Metadata.Name && ref.UID == cr.Metadata.UID
}

func deploymentDefaults(raw json.RawMessage) (defaults, error) {
	d := defaults{Length: 40, Encoding: "base64", SSHAlgorithm: "rsa", SSHLength: 2048}
	var pod struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `json:"name"`
						Args []string `json:"args"`
						Env  []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		return d, err
	}
	if len(pod.Spec.Template.Spec.Containers) == 0 {
		return d, errors.New("Deployment has no containers")
	}
	manager := -1
	for i, c := range pod.Spec.Template.Spec.Containers {
		candidate := strings.Contains(c.Name, "kubernetes-secret-generator")
		for _, env := range c.Env {
			candidate = candidate || env.Name == "WATCH_NAMESPACE" || env.Name == "OPERATOR_NAME" && env.Value == "kubernetes-secret-generator"
		}
		if candidate {
			if manager >= 0 {
				return d, errors.New("Deployment manager container is ambiguous")
			}
			manager = i
		}
	}
	if manager < 0 {
		return d, errors.New("Deployment manager container was not found")
	}
	c := pod.Spec.Template.Spec.Containers[manager]
	for _, env := range c.Env {
		switch env.Name {
		case "SECRET_LENGTH":
			d.Length = positiveInt(env.Value, d.Length)
		case "SECRET_ENCODING":
			if env.Value != "" {
				d.Encoding = env.Value
			}
		case "SSH_KEY_ALGORITHM":
			if env.Value != "" {
				d.SSHAlgorithm = strings.ToLower(env.Value)
			}
		case "SSH_KEY_LENGTH":
			d.SSHLength = positiveInt(env.Value, d.SSHLength)
		}
	}
	for i := 0; i < len(c.Args); i++ {
		name, value, ok := strings.Cut(c.Args[i], "=")
		switch name {
		case "--secret-length":
			value, i = flagValue(c.Args, i, value, ok)
			d.Length = positiveInt(value, d.Length)
		case "--secret-encoding":
			value, i = flagValue(c.Args, i, value, ok)
			if value != "" {
				d.Encoding = value
			}
		case "--ssh-key-algorithm":
			value, i = flagValue(c.Args, i, value, ok)
			if value != "" {
				d.SSHAlgorithm = strings.ToLower(value)
			}
		case "--ssh-key-length":
			value, i = flagValue(c.Args, i, value, ok)
			d.SSHLength = positiveInt(value, d.SSHLength)
		}
	}
	return d, nil
}

func flagValue(args []string, i int, inline string, hasInline bool) (string, int) {
	if hasInline {
		return inline, i
	}
	if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
		return args[i+1], i + 1
	}
	return "", i
}

func positiveInt(value string, fallback int) int {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func validateCR(cr, secret object, d defaults) []finding {
	var out []finding
	decoded, err := decodeData(secret.Data)
	if err != nil {
		return []finding{invalid(cr, "data", "managed Secret data is not valid Kubernetes base64")}
	}
	expectedType := cr.Spec.Type
	if expectedType == "" {
		expectedType = "Opaque"
	}
	actualType := secret.Type
	if actualType == "" {
		actualType = "Opaque"
	}
	if actualType != expectedType {
		out = append(out, coded(cr, "SecretTypeMismatch", "type", "Secret type differs from the immutable v4 creation-time type"))
	}
	for name, value := range cr.Spec.Data {
		if !bytes.Equal(decoded[name], []byte(value)) {
			out = append(out, overwrite(cr, "spec.data."+name, "literal data differs from the v4 desired value"))
		}
	}
	for name, value := range cr.Metadata.Labels {
		if secret.Metadata.Labels[name] != value {
			out = append(out, overwrite(cr, "metadata.labels."+name, "CR-managed label differs from the v4 desired value"))
		}
	}
	switch cr.Kind {
	case "StringSecret":
		for _, f := range cr.Spec.Fields {
			encoding := defaultString(f.Encoding, d.Encoding)
			if err := validateGenerated(decoded[f.FieldName], f.Length, encoding, d.Length); err != nil {
				out = append(out, invalid(cr, "spec.fields."+f.FieldName, "generated String value does not match effective length/encoding"))
			}
		}
	case "BasicAuth":
		username := defaultString(cr.Spec.Username, "admin")
		encoding := defaultString(cr.Spec.Encoding, d.Encoding)
		if err := validateBasicAuth(decoded, username, cr.Spec.Length, encoding, d.Length); err != nil {
			out = append(out, invalid(cr, "data", "BasicAuth username/password/bcrypt does not match the effective specification"))
		}
	case "SSHKeyPair":
		algorithm := defaultString(strings.ToLower(cr.Spec.Algorithm), d.SSHAlgorithm)
		length := sshLength(algorithm, cr.Spec.Length, d.SSHLength)
		privateField := defaultString(cr.Spec.PrivateKeyField, "ssh-privatekey")
		publicField := defaultString(cr.Spec.PublicKeyField, "ssh-publickey")
		if err := validateSSH(decoded[privateField], decoded[publicField], algorithm, length, []byte(cr.Spec.PrivateKey)); err != nil {
			out = append(out, invalid(cr, "data", "SSH private/public key does not match the effective algorithm, strength, or supplied private key"))
		}
	}
	return out
}

func validateAnnotation(secret object, d defaults) []finding {
	a := secret.Metadata.Annotations
	t := a[annotationPrefix+"type"]
	if t == "" && has(a, annotationPrefix+"autogenerate") {
		t = "string"
	}
	if t != "string" && t != "basic-auth" && t != "ssh-keypair" {
		return []finding{coded(secret, "InvalidAnnotationType", "metadata.annotations", "annotation Secret type is unknown")}
	}
	decoded, err := decodeData(secret.Data)
	if err != nil {
		return []finding{invalid(secret, "data", "managed Secret data is not valid Kubernetes base64")}
	}
	length := a[annotationPrefix+"length"]
	encoding := defaultString(a[annotationPrefix+"encoding"], d.Encoding)
	switch t {
	case "string":
		for _, name := range strings.Split(a[annotationPrefix+"autogenerate"], ",") {
			if err := validateGenerated(decoded[name], length, encoding, d.Length); err != nil {
				return []finding{invalid(secret, "metadata.annotations", "annotation String value does not match effective length/encoding")}
			}
		}
	case "basic-auth":
		username := defaultString(a[annotationPrefix+"basic-auth-username"], "admin")
		if !utf8.ValidString(username) || utf8.RuneCountInString(username) < 1 || utf8.RuneCountInString(username) > 255 || strings.ContainsAny(username, ":\r\n\x00") {
			return []finding{coded(secret, "InvalidUsername", "metadata.annotations", "annotation BasicAuth effective username is invalid")}
		}
		if n, byteLength, parseErr := parseLength(length, d.Length); parseErr != nil {
			return []finding{coded(secret, "InvalidAnnotationLength", "metadata.annotations", "annotation BasicAuth length is invalid")}
		} else if generatedLength(n, byteLength, encoding) > 72 {
			return []finding{coded(secret, "BcryptInputTooLong", "metadata.annotations", "annotation BasicAuth encoded password exceeds the bcrypt limit")}
		}
		if err := validateBasicAuth(decoded, username, length, encoding, d.Length); err != nil {
			return []finding{invalid(secret, "data", "annotation BasicAuth username/password/bcrypt does not match the effective configuration")}
		}
	case "ssh-keypair":
		algorithm := defaultString(strings.ToLower(a[annotationPrefix+"ssh-key-algorithm"]), d.SSHAlgorithm)
		privateField := defaultString(a[annotationPrefix+"private-key-field"], "ssh-privatekey")
		publicField := defaultString(a[annotationPrefix+"public-key-field"], "ssh-publickey")
		if err := validateSSH(decoded[privateField], decoded[publicField], algorithm, sshLength(algorithm, length, d.SSHLength), nil); err != nil {
			return []finding{invalid(secret, "data", "annotation SSH private/public key does not match the effective algorithm and strength")}
		}
	}
	return nil
}

func decodeData(data map[string]string) (map[string][]byte, error) {
	decoded := make(map[string][]byte, len(data))
	for name, value := range data {
		b, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, err
		}
		decoded[name] = b
	}
	return decoded, nil
}

func validateGenerated(value []byte, lengthSpec, encoding string, fallback int) error {
	n, byteLength, err := parseLength(lengthSpec, fallback)
	if err != nil || len(value) == 0 {
		return errors.New("invalid length or empty value")
	}
	expected := generatedLength(n, byteLength, encoding)
	if expected < 0 {
		return errors.New("unsupported encoding")
	}
	if len(value) != expected {
		return errors.New("length mismatch")
	}
	if encoding == "raw" {
		return nil
	}
	if !validEncodedAlphabet(value, encoding) {
		return errors.New("encoding mismatch")
	}
	if !byteLength {
		return nil
	}
	var decoded []byte
	switch encoding {
	case "base64":
		decoded, err = base64.StdEncoding.DecodeString(string(value))
	case "base64url":
		decoded, err = base64.URLEncoding.DecodeString(string(value))
	case "base32":
		decoded, err = base32.StdEncoding.DecodeString(string(value))
	case "hex":
		decoded, err = hex.DecodeString(string(value))
	}
	if err != nil || len(decoded) != n {
		return errors.New("encoded byte length mismatch")
	}
	return nil
}

func generatedLength(n int, byteLength bool, encoding string) int {
	if !byteLength {
		return n
	}
	switch encoding {
	case "base64", "base64url":
		return base64.StdEncoding.EncodedLen(n)
	case "base32":
		return base32.StdEncoding.EncodedLen(n)
	case "hex":
		return hex.EncodedLen(n)
	case "raw":
		return n
	default:
		return -1
	}
}

func parseLength(value string, fallback int) (int, bool, error) {
	if value == "" {
		return fallback, false, nil
	}
	byteLength := strings.HasSuffix(strings.ToLower(value), "b")
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(value, "b"), "B"))
	if err != nil || n < 1 || n > 65536 {
		return 0, byteLength, errors.New("invalid length")
	}
	return n, byteLength, nil
}

func validEncodedAlphabet(value []byte, encoding string) bool {
	text := string(value)
	padding := strings.IndexByte(text, '=')
	if padding >= 0 && strings.Trim(text[padding:], "=") != "" {
		return false
	}
	if padding >= 0 {
		limit := 0
		switch encoding {
		case "base64", "base64url":
			limit = 2
		case "base32":
			limit = 6
		}
		if limit == 0 || len(text)-padding > limit {
			return false
		}
	}
	for _, c := range text {
		if c == '=' {
			continue
		}
		valid := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		switch encoding {
		case "base64":
			valid = valid || c == '+' || c == '/'
		case "base64url":
			valid = valid || c == '-' || c == '_'
		case "base32":
			valid = c >= 'A' && c <= 'Z' || c >= '2' && c <= '7'
		case "hex":
			valid = c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
		default:
			return false
		}
		if !valid {
			return false
		}
	}
	return true
}

func validateBasicAuth(data map[string][]byte, username, length, encoding string, fallback int) error {
	if !utf8.ValidString(username) || username == "" || strings.ContainsAny(username, ":\r\n\x00") {
		return errors.New("invalid username")
	}
	if !bytes.Equal(data["username"], []byte(username)) || validateGenerated(data["password"], length, encoding, fallback) != nil {
		return errors.New("username or password mismatch")
	}
	prefix := append([]byte(username), ':')
	if !bytes.HasPrefix(data["auth"], prefix) || bcrypt.CompareHashAndPassword(data["auth"][len(prefix):], data["password"]) != nil {
		return errors.New("bcrypt mismatch")
	}
	return nil
}

func sshLength(algorithm, value string, fallback int) int {
	if algorithm == "ed25519" {
		return 0
	}
	if value != "" {
		return positiveInt(value, fallback)
	}
	if algorithm == "ecdsa" {
		return 256
	}
	return fallback
}

func validateSSH(privatePEM, public []byte, algorithm string, length int, supplied []byte) error {
	if len(privatePEM) == 0 || len(privatePEM) > 65536 || len(public) == 0 {
		return errors.New("missing or oversized key")
	}
	key, err := parseSSHPrivate(privatePEM, algorithm, length)
	if err != nil {
		return err
	}
	if len(supplied) > 0 && !bytes.Equal(supplied, privatePEM) {
		return errors.New("supplied key mismatch")
	}
	pub, err := ssh.NewPublicKey(publicKey(key))
	if err != nil || !bytes.Equal(ssh.MarshalAuthorizedKey(pub), public) {
		return errors.New("public key mismatch")
	}
	return nil
}

func parseSSHPrivate(privatePEM []byte, algorithm string, length int) (any, error) {
	if len(privatePEM) == 0 || len(privatePEM) > 65536 {
		return nil, errors.New("missing or oversized key")
	}
	key, err := ssh.ParseRawPrivateKey(privatePEM)
	if err != nil {
		return nil, err
	}
	switch algorithm {
	case "rsa":
		k, ok := key.(*rsa.PrivateKey)
		if !ok || k.N.BitLen() != length || (length != 2048 && length != 3072 && length != 4096) || k.Validate() != nil {
			return nil, errors.New("RSA mismatch")
		}
	case "ecdsa":
		k, ok := key.(*ecdsa.PrivateKey)
		if !ok || k.Curve.Params().BitSize != length || (length != 256 && length != 384 && length != 521) {
			return nil, errors.New("ECDSA mismatch")
		}
	case "ed25519":
		switch key.(type) {
		case ed25519.PrivateKey, *ed25519.PrivateKey:
		default:
			return nil, errors.New("Ed25519 mismatch")
		}
	default:
		return nil, errors.New("unsupported algorithm")
	}
	return key, nil
}

func publicKey(key any) any {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	case ed25519.PrivateKey:
		return k.Public()
	case *ed25519.PrivateKey:
		return (*k).Public()
	default:
		return nil
	}
}

func annotationManaged(o object) bool {
	a := o.Metadata.Annotations
	return has(a, annotationPrefix+"autogenerate") || a[annotationPrefix+"type"] != ""
}

func has(values map[string]string, name string) bool { _, ok := values[name]; return ok }
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func invalid(o object, field, message string) finding {
	return finding{Severity: "blocker", Code: "LegacyBaselineInvalid", Kind: o.Kind, Namespace: o.Metadata.Namespace, Name: o.Metadata.Name, Field: field, Message: message}
}

func overwrite(o object, field, message string) finding {
	return finding{Severity: "blocker", Code: "DeclarativeOverwritePending", Kind: o.Kind, Namespace: o.Metadata.Namespace, Name: o.Metadata.Name, Field: field, Message: message}
}

func coded(o object, code, field, message string) finding {
	return finding{Severity: "blocker", Code: code, Kind: o.Kind, Namespace: o.Metadata.Namespace, Name: o.Metadata.Name, Field: field, Message: message}
}
