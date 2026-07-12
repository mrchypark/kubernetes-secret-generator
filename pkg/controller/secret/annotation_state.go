package secret

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
)

const (
	reasonReconciled              = "Reconciled"
	reasonInvalidSpec             = "InvalidSpec"
	reasonLegacyBaselineInvalid   = "LegacyBaselineInvalid"
	reasonTrackingStateConflict   = "TrackingStateConflict"
	reasonSecretSizeConflict      = "SecretSizeConflict"
	reasonImmutableSecretConflict = "ImmutableSecretConflict"
	reasonGenerationFailed        = "GenerationFailed"
)

var annotationTrackingKeys = []string{
	AnnotationTrackingVersion,
	AnnotationManagedDataKeys,
	AnnotationManagedLabelKeys,
	AnnotationManagedDataChecksums,
	AnnotationGenerationFingerprint,
	AnnotationManagedKeysDigest,
	AnnotationManagedBy,
}

type annotationTrackingState int

const (
	annotationTrackingAbsent annotationTrackingState = iota
	annotationTrackingValid
	annotationTrackingChecksumIncomplete
	annotationTrackingConflict
)

type annotationTracking struct {
	DataKeys     []string
	Checksums    map[string]string
	Fingerprints map[string]string
}

type annotationPlan struct {
	kind         Type
	fields       []string
	length       int
	byteLength   bool
	encoding     string
	username     string
	algorithm    string
	sshLength    int
	privateField string
	publicField  string
	fingerprints map[string]string
	projected    map[string]int
	regenerate   map[string]bool
	requested    bool
}

type annotationOutcome struct {
	desired  *corev1.Secret
	reason   string
	changed  bool
	rotated  bool
	terminal bool
}

func annotationFingerprint(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		var n [8]byte
		v := uint64(len(part))
		for i := len(n) - 1; i >= 0; i-- {
			n[i] = byte(v)
			v >>= 8
		}
		_, _ = h.Write(n[:])
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func annotationDataChecksum(key string, value []byte) string {
	return annotationFingerprint([]byte(key), value)
}

func buildAnnotationPlan(instance *corev1.Secret) (*annotationPlan, error) {
	a := instance.Annotations
	kind := Type(a[AnnotationSecretType])
	if kind == "" {
		if _, ok := a[AnnotationSecretAutoGenerate]; !ok {
			return nil, nil
		}
		kind = TypeString
	}
	if err := kind.Validate(); err != nil {
		return nil, validationError("type", "unsupported Secret generator type")
	}

	p := &annotationPlan{kind: kind, fingerprints: map[string]string{}, regenerate: map[string]bool{}}
	switch kind {
	case TypeString:
		p.fields = strings.Split(a[AnnotationSecretAutoGenerate], ",")
		if err := ValidateManagedFields(p.fields, nil); err != nil {
			return nil, err
		}
		length, byteLength, encoding, err := annotationStringConfiguration(a)
		if err != nil {
			return nil, err
		}
		p.length, p.byteLength, p.encoding = length, byteLength, encoding
		valueLength, err := EncodedOutputLength(length, encoding, byteLength)
		if err != nil {
			return nil, err
		}
		p.projected = make(map[string]int, len(p.fields))
		for _, field := range p.fields {
			p.fingerprints[field] = annotationFingerprint([]byte(field), []byte(strconv.Itoa(length)), []byte(strconv.FormatBool(byteLength)), []byte(encoding))
			p.projected[field] = valueLength
		}
	case TypeBasicAuth:
		username := a[AnnotationBasicAuthUsername]
		length, byteLength, encoding, err := annotationStringConfiguration(a)
		if err != nil {
			return nil, err
		}
		constraints := &BasicAuthConstraints{Username: username, Encoding: encoding, Length: annotationLength(a, DefaultLength())}
		projected, err := BasicAuthProjectedValueLengths(constraints)
		if err != nil {
			return nil, err
		}
		p.fields = []string{FieldBasicAuthIngress, FieldBasicAuthPassword, FieldBasicAuthUsername}
		p.length, p.byteLength, p.encoding, p.username, p.projected = length, byteLength, encoding, constraints.Username, projected
		p.fingerprints["credential-set"] = annotationFingerprint([]byte(strconv.Itoa(length)), []byte(strconv.FormatBool(byteLength)), []byte(encoding), []byte(constraints.Username))
	case TypeSSHKeypair:
		privateField, err := GetPrivateKeyFieldFromAnnotation(DefaultSecretFieldPrivateKey, a)
		if err != nil {
			return nil, err
		}
		publicField, err := GetPublicKeyFieldFromAnnotation(DefaultSecretFieldPublicKey, a)
		if err != nil {
			return nil, err
		}
		if err := ValidateSSHFields(privateField, publicField, nil); err != nil {
			return nil, err
		}
		algorithm := normalizeSSHKeyAlgorithm(a[AnnotationSSHKeyAlgorithm])
		if algorithm == "" {
			algorithm = normalizeSSHKeyAlgorithm(SSHKeyAlgorithm())
		}
		length := annotationLength(a, SSHKeyLength())
		if _, ok := a[AnnotationSecretLength]; !ok {
			switch algorithm {
			case SSHKeyAlgorithmECDSA:
				length = "256"
			case SSHKeyAlgorithmED25519:
				length = ""
			}
		}
		algorithm, expected, err := ValidateSSHConfiguration(algorithm, length)
		if err != nil {
			return nil, err
		}
		projected, err := SSHProjectedValueLengths(algorithm, expected, privateField, publicField, true, instance.Data)
		if err != nil {
			return nil, err
		}
		p.fields = []string{privateField, publicField}
		p.algorithm, p.sshLength, p.privateField, p.publicField, p.projected = algorithm, expected, privateField, publicField, projected
		p.fingerprints["key-set"] = annotationFingerprint([]byte(algorithm), []byte(strconv.Itoa(expected)), []byte(privateField), []byte(publicField))
	}

	value, requested := a[AnnotationSecretRegenerate]
	p.requested = requested
	selected, err := ParseRegenerate(value, p.fields, kind == TypeString)
	if err != nil {
		return nil, err
	}
	for _, field := range selected {
		p.regenerate[field] = true
	}
	if kind == TypeString && RegenerateInsecure() && a[AnnotationSecretSecure] == "" {
		for _, field := range p.fields {
			p.regenerate[field] = true
		}
	}
	return p, nil
}

func annotationLength(annotations map[string]string, fallback int) string {
	if value, ok := annotations[AnnotationSecretLength]; ok {
		return value
	}
	return strconv.Itoa(fallback)
}

func annotationStringConfiguration(annotations map[string]string) (int, bool, string, error) {
	length, byteLength, err := ParseByteLength(DefaultLength(), annotationLength(annotations, DefaultLength()))
	if err != nil {
		return 0, false, "", err
	}
	encoding, err := getEncodingFromAnnotation(DefaultEncoding(), annotations)
	if err != nil {
		return 0, false, "", err
	}
	return length, byteLength, encoding, nil
}

func loadAnnotationTracking(secret *corev1.Secret) (*annotationTracking, annotationTrackingState, error) {
	present := 0
	for _, key := range annotationTrackingKeys {
		if _, ok := secret.Annotations[key]; ok {
			present++
		}
	}
	if present == 0 {
		return nil, annotationTrackingAbsent, nil
	}
	if present != len(annotationTrackingKeys) || secret.Annotations[AnnotationTrackingVersion] != TrackingVersion || secret.Annotations[AnnotationManagedBy] != AnnotationControllerMarker {
		return nil, annotationTrackingConflict, fmt.Errorf("tracking bundle is partial or unsupported")
	}
	t := &annotationTracking{}
	if err := json.Unmarshal([]byte(secret.Annotations[AnnotationManagedDataKeys]), &t.DataKeys); err != nil || !canonicalAnnotationList(t.DataKeys) {
		return nil, annotationTrackingConflict, fmt.Errorf("managed key list is malformed")
	}
	var labelKeys []string
	if err := json.Unmarshal([]byte(secret.Annotations[AnnotationManagedLabelKeys]), &labelKeys); err != nil || len(labelKeys) != 0 {
		return nil, annotationTrackingConflict, fmt.Errorf("managed label list is malformed")
	}
	if annotationFingerprint(mustAnnotationJSON(t.DataKeys), mustAnnotationJSON(labelKeys)) != secret.Annotations[AnnotationManagedKeysDigest] {
		return nil, annotationTrackingConflict, fmt.Errorf("managed key digest does not match")
	}
	if err := json.Unmarshal([]byte(secret.Annotations[AnnotationGenerationFingerprint]), &t.Fingerprints); err != nil || t.Fingerprints == nil {
		return nil, annotationTrackingConflict, fmt.Errorf("generation fingerprints are malformed")
	}
	for key, value := range t.Fingerprints {
		if key == "" || !validAnnotationSHA(value) {
			return nil, annotationTrackingConflict, fmt.Errorf("generation fingerprint entry is malformed")
		}
	}
	if err := json.Unmarshal([]byte(secret.Annotations[AnnotationManagedDataChecksums]), &t.Checksums); err != nil || t.Checksums == nil {
		t.Checksums = map[string]string{}
		return t, annotationTrackingChecksumIncomplete, fmt.Errorf("managed checksums are malformed")
	}
	incomplete := false
	for key, value := range t.Checksums {
		if key == "" || !validAnnotationSHA(value) {
			delete(t.Checksums, key)
			incomplete = true
		}
	}
	for _, key := range t.DataKeys {
		if _, ok := t.Checksums[key]; !ok {
			incomplete = true
		}
	}
	if incomplete {
		return t, annotationTrackingChecksumIncomplete, fmt.Errorf("managed checksum entry is missing")
	}
	return t, annotationTrackingValid, nil
}

func storeAnnotationTracking(secret *corev1.Secret, tracking *annotationTracking) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	tracking.DataKeys = sortedAnnotationKeys(tracking.DataKeys)
	labelKeys := []string{}
	secret.Annotations[AnnotationTrackingVersion] = TrackingVersion
	secret.Annotations[AnnotationManagedDataKeys] = string(mustAnnotationJSON(tracking.DataKeys))
	secret.Annotations[AnnotationManagedLabelKeys] = string(mustAnnotationJSON(labelKeys))
	secret.Annotations[AnnotationManagedDataChecksums] = string(mustAnnotationJSON(tracking.Checksums))
	secret.Annotations[AnnotationGenerationFingerprint] = string(mustAnnotationJSON(tracking.Fingerprints))
	secret.Annotations[AnnotationManagedKeysDigest] = annotationFingerprint(mustAnnotationJSON(tracking.DataKeys), mustAnnotationJSON(labelKeys))
	secret.Annotations[AnnotationManagedBy] = AnnotationControllerMarker
}

func reconcileAnnotationSecret(instance *corev1.Secret, plan *annotationPlan) (annotationOutcome, error) {
	desired := instance.DeepCopy()
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	if desired.Data == nil {
		desired.Data = map[string][]byte{}
	}
	if desired.Annotations[AnnotationSecretType] == "" {
		desired.Annotations[AnnotationSecretType] = string(plan.kind)
	}

	tracking, state, _ := loadAnnotationTracking(instance)
	if state == annotationTrackingConflict {
		if instance.Immutable != nil && *instance.Immutable {
			return annotationOutcome{reason: reasonImmutableSecretConflict, terminal: true}, nil
		}
		return annotationOutcome{reason: reasonTrackingStateConflict, terminal: true}, nil
	}
	if tracking == nil {
		tracking = &annotationTracking{Checksums: map[string]string{}, Fingerprints: map[string]string{}}
	}

	rotate, publicRepair, baselineInvalid := plan.rotation(instance, tracking, state)
	if baselineInvalid {
		return annotationOutcome{reason: reasonLegacyBaselineInvalid, terminal: true}, nil
	}
	for field := range plan.regenerate {
		rotate[field] = true
	}

	trackingMutation := state != annotationTrackingValid || !reflect.DeepEqual(sortedAnnotationKeys(tracking.DataKeys), sortedAnnotationKeys(plan.fields)) || !reflect.DeepEqual(tracking.Fingerprints, plan.fingerprints)
	annotationMutation := plan.requested && len(plan.regenerate) == 0
	dataMutation := len(rotate) > 0 || publicRepair
	mutationRequired := trackingMutation || annotationMutation || dataMutation || desired.Annotations[AnnotationSecretType] != instance.Annotations[AnnotationSecretType]
	if instance.Immutable != nil && *instance.Immutable && mutationRequired {
		return annotationOutcome{reason: reasonImmutableSecretConflict, terminal: true}, nil
	}
	if state != annotationTrackingAbsent && sameAnnotationKeys(tracking.DataKeys, plan.fields) {
		for key := range plan.fingerprints {
			if _, ok := tracking.Fingerprints[key]; !ok {
				return annotationOutcome{reason: reasonTrackingStateConflict, terminal: true}, nil
			}
		}
	}
	if !mutationRequired {
		return annotationOutcome{reason: reasonReconciled}, nil
	}
	next := &annotationTracking{DataKeys: append([]string(nil), plan.fields...), Checksums: map[string]string{}, Fingerprints: cloneAnnotationMap(plan.fingerprints)}
	for _, key := range next.DataKeys {
		next.Checksums[key] = annotationDataChecksum(key, nil)
	}
	storeAnnotationTracking(desired, next)
	if dataMutation {
		// Account for the fixed-width marker before allocating generated values.
		desired.Annotations[AnnotationSecretAutoGeneratedAt] = "0000-00-00T00:00:00Z"
	}
	projected := map[string]int{}
	for key := range rotate {
		projected[key] = plan.projected[key]
	}
	if publicRepair {
		projected[plan.publicField] = plan.projected[plan.publicField]
	}
	if state == annotationTrackingAbsent {
		for _, key := range plan.fields {
			if len(instance.Data[key]) == 0 {
				projected[key] = plan.projected[key]
			}
		}
	}
	if err := ValidateProjectedDataSize(desired, projected); err != nil {
		if IsValidationError(err) {
			return annotationOutcome{reason: reasonSecretSizeConflict, terminal: true}, nil
		}
		return annotationOutcome{}, err
	}

	rotated := false
	switch plan.kind {
	case TypeString:
		allSecure := true
		for _, field := range plan.fields {
			if !rotate[field] && len(desired.Data[field]) != 0 {
				if instance.Annotations[AnnotationSecretSecure] == "" {
					allSecure = false
				}
				continue
			}
			value, err := GenerateRandomString(plan.length, plan.encoding, plan.byteLength)
			if err != nil {
				return annotationOutcome{}, err
			}
			desired.Data[field] = value
			rotated = true
		}
		if allSecure {
			desired.Annotations[AnnotationSecretSecure] = "yes"
		}
	case TypeBasicAuth:
		if len(rotate) > 0 || missingAny(instance.Data, plan.fields) {
			constraints := &BasicAuthConstraints{Username: plan.username, Encoding: plan.encoding, Length: annotationLength(instance.Annotations, DefaultLength())}
			if err := GenerateBasicAuthData(logr.Discard(), constraints, desired.Data); err != nil {
				return annotationOutcome{}, err
			}
			rotated = true
		}
	case TypeSSHKeypair:
		if len(rotate) > 0 || len(desired.Data[plan.privateField]) == 0 {
			length := strconv.Itoa(plan.sshLength)
			if plan.algorithm == SSHKeyAlgorithmED25519 {
				length = ""
			}
			if err := GenerateSSHKeypairDataWithAlgorithm(logr.Discard(), plan.algorithm, length, plan.privateField, plan.publicField, true, desired.Data); err != nil {
				return annotationOutcome{}, err
			}
			rotated = true
		} else if publicRepair || len(desired.Data[plan.publicField]) == 0 {
			key, err := ValidateSSHPrivateKey(desired.Data[plan.privateField], plan.algorithm, plan.sshLength)
			if err != nil {
				return annotationOutcome{}, err
			}
			public, err := SSHPublicKeyForPrivateKey(key)
			if err != nil {
				return annotationOutcome{}, err
			}
			desired.Data[plan.publicField] = public
		}
	}

	next.Checksums = make(map[string]string, len(next.DataKeys))
	for _, key := range next.DataKeys {
		next.Checksums[key] = annotationDataChecksum(key, desired.Data[key])
	}
	storeAnnotationTracking(desired, next)
	if plan.requested {
		delete(desired.Annotations, AnnotationSecretRegenerate)
	}
	if !reflect.DeepEqual(instance.Data, desired.Data) {
		desired.Annotations[AnnotationSecretAutoGeneratedAt] = time.Now().UTC().Format(time.RFC3339)
	}
	if err := ValidateProjectedSecretSize(desired); err != nil {
		if IsValidationError(err) {
			return annotationOutcome{reason: reasonSecretSizeConflict, terminal: true}, nil
		}
		return annotationOutcome{}, err
	}
	changed := !reflect.DeepEqual(instance.Annotations, desired.Annotations) || !reflect.DeepEqual(instance.Data, desired.Data)
	return annotationOutcome{desired: desired, reason: reasonReconciled, changed: changed, rotated: rotated}, nil
}

func (p *annotationPlan) rotation(instance *corev1.Secret, tracking *annotationTracking, state annotationTrackingState) (map[string]bool, bool, bool) {
	rotate := map[string]bool{}
	if state == annotationTrackingAbsent {
		return p.baselineRotation(instance)
	}
	switch p.kind {
	case TypeString:
		for _, field := range p.fields {
			checksum, ok := tracking.Checksums[field]
			if tracking.Fingerprints[field] != p.fingerprints[field] || !ok || checksum != annotationDataChecksum(field, instance.Data[field]) {
				rotate[field] = true
			}
		}
	case TypeBasicAuth:
		changed := tracking.Fingerprints["credential-set"] != p.fingerprints["credential-set"]
		for _, field := range p.fields {
			checksum, ok := tracking.Checksums[field]
			changed = changed || !ok || checksum != annotationDataChecksum(field, instance.Data[field])
		}
		if changed {
			for _, field := range p.fields {
				rotate[field] = true
			}
		}
	case TypeSSHKeypair:
		fingerprintChanged := tracking.Fingerprints["key-set"] != p.fingerprints["key-set"]
		privateChecksum, privateOK := tracking.Checksums[p.privateField]
		publicChecksum, publicOK := tracking.Checksums[p.publicField]
		privateDrift := !privateOK || privateChecksum != annotationDataChecksum(p.privateField, instance.Data[p.privateField])
		publicDrift := !publicOK || publicChecksum != annotationDataChecksum(p.publicField, instance.Data[p.publicField])
		if fingerprintChanged || privateDrift {
			rotate[p.privateField], rotate[p.publicField] = true, true
			return rotate, false, false
		}
		return rotate, publicDrift, false
	}
	return rotate, false, false
}

func (p *annotationPlan) baselineRotation(instance *corev1.Secret) (map[string]bool, bool, bool) {
	rotate := map[string]bool{}
	switch p.kind {
	case TypeString:
		invalid := false
		for _, field := range p.fields {
			value := instance.Data[field]
			if len(value) == 0 {
				rotate[field] = true
				continue
			}
			invalid = invalid || !p.regenerate[field] && validateAnnotationString(value, p.length, p.byteLength, p.encoding) != nil
		}
		return rotate, false, invalid
	case TypeBasicAuth:
		if missingAny(instance.Data, p.fields) {
			for _, field := range p.fields {
				rotate[field] = true
			}
			return rotate, false, false
		}
		invalid := string(instance.Data[FieldBasicAuthUsername]) != p.username || validateAnnotationString(instance.Data[FieldBasicAuthPassword], p.length, p.byteLength, p.encoding) != nil || ValidateBasicAuthCredential(instance.Data) != nil
		if len(p.regenerate) > 0 {
			invalid = false
		}
		return rotate, false, invalid
	case TypeSSHKeypair:
		private := instance.Data[p.privateField]
		if len(private) == 0 {
			rotate[p.privateField], rotate[p.publicField] = true, true
			return rotate, false, false
		}
		key, err := ValidateSSHPrivateKey(private, p.algorithm, p.sshLength)
		if err != nil {
			if len(p.regenerate) > 0 {
				return rotate, false, false
			}
			return rotate, false, true
		}
		public, err := SSHPublicKeyForPrivateKey(key)
		if err != nil {
			return rotate, false, true
		}
		if len(p.regenerate) > 0 {
			return rotate, false, false
		}
		return rotate, !bytes.Equal(instance.Data[p.publicField], public), false
	}
	return rotate, false, false
}

func validateAnnotationString(value []byte, length int, byteLength bool, encoding string) error {
	want, err := EncodedOutputLength(length, encoding, byteLength)
	if err != nil {
		return err
	}
	if len(value) != want {
		return fmt.Errorf("generated value does not match effective configuration")
	}
	if encoding == "raw" {
		return nil
	}
	if !byteLength {
		for _, c := range value {
			valid := false
			switch encoding {
			case "base64":
				valid = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
			case "base64url":
				valid = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
			case "base32":
				valid = c >= 'A' && c <= 'Z' || c >= '2' && c <= '7'
			case "hex":
				valid = c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
			}
			if !valid {
				return fmt.Errorf("generated value does not match effective configuration")
			}
		}
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
	if err != nil || len(decoded) != length {
		return fmt.Errorf("generated value does not match effective configuration")
	}
	return nil
}

func missingAny(data map[string][]byte, fields []string) bool {
	for _, field := range fields {
		if len(data[field]) == 0 {
			return true
		}
	}
	return false
}

func canonicalAnnotationList(values []string) bool {
	if values == nil {
		return false
	}
	for i, value := range values {
		if value == "" || i > 0 && values[i-1] >= value {
			return false
		}
	}
	return true
}

func sortedAnnotationKeys(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sameAnnotationKeys(a, b []string) bool {
	return reflect.DeepEqual(sortedAnnotationKeys(a), sortedAnnotationKeys(b))
}

func validAnnotationSHA(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func mustAnnotationJSON(value interface{}) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func cloneAnnotationMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
