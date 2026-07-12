package secret

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func annotationTestSecret(kind Type) *corev1.Secret {
	annotations := map[string]string{AnnotationSecretType: string(kind)}
	if kind == TypeString {
		annotations[AnnotationSecretAutoGenerate] = "value"
	}
	if kind == TypeSSHKeypair {
		annotations[AnnotationSSHKeyAlgorithm] = SSHKeyAlgorithmED25519
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: types.UID("uid-test"), Annotations: annotations}, Data: map[string][]byte{}}
}

func reconcileAnnotationForTest(t *testing.T, instance *corev1.Secret) annotationOutcome {
	t.Helper()
	plan, err := buildAnnotationPlan(instance)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := reconcileAnnotationSecret(instance, plan)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestAnnotationStringTrackingDriftFingerprintAndNoop(t *testing.T) {
	setupAnnotationDefaults(t)
	instance := annotationTestSecret(TypeString)
	created := reconcileAnnotationForTest(t, instance)
	if !created.changed || !created.rotated {
		t.Fatal("initial reconcile did not generate and track data")
	}
	if created.desired.Annotations[AnnotationManagedBy] != AnnotationControllerMarker {
		t.Fatal("controller marker missing")
	}
	initial := append([]byte(nil), created.desired.Data["value"]...)

	noop := reconcileAnnotationForTest(t, created.desired)
	if noop.changed || noop.rotated {
		t.Fatal("stable reconcile was not a semantic no-op")
	}

	drifted := created.desired.DeepCopy()
	drifted.Data["value"] = []byte("tampered")
	repaired := reconcileAnnotationForTest(t, drifted)
	if !repaired.changed || !repaired.rotated || bytes.Equal(repaired.desired.Data["value"], drifted.Data["value"]) {
		t.Fatal("checksum drift did not rotate the affected generated field")
	}

	changedSpec := created.desired.DeepCopy()
	changedSpec.Annotations[AnnotationSecretLength] = "41"
	changed := reconcileAnnotationForTest(t, changedSpec)
	if !changed.rotated || bytes.Equal(changed.desired.Data["value"], initial) || len(changed.desired.Data["value"]) != 41 {
		t.Fatal("generation fingerprint change did not rotate once")
	}
	if again := reconcileAnnotationForTest(t, changed.desired); again.changed {
		t.Fatal("fingerprint reconcile was not stable after the first write")
	}
}

func TestAnnotationBasicAuthDriftRotatesWholeCredentialSet(t *testing.T) {
	setupAnnotationDefaults(t)
	created := reconcileAnnotationForTest(t, annotationTestSecret(TypeBasicAuth)).desired
	oldUsername := append([]byte(nil), created.Data[FieldBasicAuthUsername]...)
	oldPassword := append([]byte(nil), created.Data[FieldBasicAuthPassword]...)
	oldAuth := append([]byte(nil), created.Data[FieldBasicAuthIngress]...)

	drifted := created.DeepCopy()
	drifted.Data[FieldBasicAuthUsername] = []byte("tampered")
	repaired := reconcileAnnotationForTest(t, drifted)
	if !repaired.rotated || bytes.Equal(repaired.desired.Data[FieldBasicAuthPassword], oldPassword) || bytes.Equal(repaired.desired.Data[FieldBasicAuthIngress], oldAuth) {
		t.Fatal("BasicAuth drift did not rotate the complete credential set")
	}
	if !bytes.Equal(repaired.desired.Data[FieldBasicAuthUsername], oldUsername) {
		t.Fatal("BasicAuth rotation did not restore the effective username")
	}
}

func TestAnnotationBasicAuthFingerprintChangeRotatesOnce(t *testing.T) {
	setupAnnotationDefaults(t)
	created := reconcileAnnotationForTest(t, annotationTestSecret(TypeBasicAuth)).desired
	oldPassword := append([]byte(nil), created.Data[FieldBasicAuthPassword]...)
	created.Annotations[AnnotationBasicAuthUsername] = "new-user"
	changed := reconcileAnnotationForTest(t, created)
	if !changed.rotated || bytes.Equal(changed.desired.Data[FieldBasicAuthPassword], oldPassword) || string(changed.desired.Data[FieldBasicAuthUsername]) != "new-user" {
		t.Fatal("BasicAuth generation fingerprint change did not rotate the credential set")
	}
	if again := reconcileAnnotationForTest(t, changed.desired); again.changed {
		t.Fatal("BasicAuth fingerprint change rotated more than once")
	}
}

func TestAnnotationStringSelectiveAndNoopRegeneration(t *testing.T) {
	setupAnnotationDefaults(t)
	instance := annotationTestSecret(TypeString)
	instance.Annotations[AnnotationSecretAutoGenerate] = "one,two"
	created := reconcileAnnotationForTest(t, instance).desired
	one, two := append([]byte(nil), created.Data["one"]...), append([]byte(nil), created.Data["two"]...)

	created.Annotations[AnnotationSecretRegenerate] = "one"
	selective := reconcileAnnotationForTest(t, created)
	if bytes.Equal(selective.desired.Data["one"], one) || !bytes.Equal(selective.desired.Data["two"], two) {
		t.Fatal("selective regeneration changed the wrong fields")
	}
	if _, ok := selective.desired.Annotations[AnnotationSecretRegenerate]; ok {
		t.Fatal("successful selective regeneration request was retained")
	}

	selective.desired.Annotations[AnnotationSecretRegenerate] = "false"
	noRotation := reconcileAnnotationForTest(t, selective.desired)
	if noRotation.rotated || !bytes.Equal(noRotation.desired.Data["one"], selective.desired.Data["one"]) || !bytes.Equal(noRotation.desired.Data["two"], selective.desired.Data["two"]) {
		t.Fatal("false regeneration request rotated generated data")
	}
	if _, ok := noRotation.desired.Annotations[AnnotationSecretRegenerate]; ok {
		t.Fatal("false regeneration request was not removed")
	}
}

func TestAnnotationSSHRepairsPublicAndRotatesPrivateDrift(t *testing.T) {
	setupAnnotationDefaults(t)
	created := reconcileAnnotationForTest(t, annotationTestSecret(TypeSSHKeypair)).desired
	private := append([]byte(nil), created.Data[DefaultSecretFieldPrivateKey]...)

	publicDrift := created.DeepCopy()
	publicDrift.Data[DefaultSecretFieldPublicKey] = []byte("tampered")
	publicRepair := reconcileAnnotationForTest(t, publicDrift)
	if !bytes.Equal(publicRepair.desired.Data[DefaultSecretFieldPrivateKey], private) {
		t.Fatal("public-only drift rotated the private key")
	}
	key, err := ValidateSSHPrivateKey(private, SSHKeyAlgorithmED25519, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantPublic, err := SSHPublicKeyForPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(publicRepair.desired.Data[DefaultSecretFieldPublicKey], wantPublic) {
		t.Fatal("public-only drift was not derived from the current private key")
	}

	privateDrift := created.DeepCopy()
	privateDrift.Data[DefaultSecretFieldPrivateKey] = []byte("tampered")
	privateRepair := reconcileAnnotationForTest(t, privateDrift)
	if !privateRepair.rotated || bytes.Equal(privateRepair.desired.Data[DefaultSecretFieldPrivateKey], private) {
		t.Fatal("private-key drift did not rotate the pair")
	}
}

func TestAnnotationSSHFingerprintChangeRotatesOnce(t *testing.T) {
	setupAnnotationDefaults(t)
	created := reconcileAnnotationForTest(t, annotationTestSecret(TypeSSHKeypair)).desired
	private := append([]byte(nil), created.Data[DefaultSecretFieldPrivateKey]...)
	created.Annotations[AnnotationSSHPublicKeyField] = "custom-public"
	changed := reconcileAnnotationForTest(t, created)
	if !changed.rotated || bytes.Equal(changed.desired.Data[DefaultSecretFieldPrivateKey], private) || len(changed.desired.Data["custom-public"]) == 0 {
		t.Fatal("SSH generation fingerprint change did not rotate into the desired fields")
	}
	if again := reconcileAnnotationForTest(t, changed.desired); again.changed {
		t.Fatal("SSH fingerprint change rotated more than once")
	}
}

func TestAnnotationImmutableBlocksTrackingAndDataMutation(t *testing.T) {
	setupAnnotationDefaults(t)
	instance := annotationTestSecret(TypeString)
	immutable := true
	instance.Immutable = &immutable
	original := instance.DeepCopy()
	outcome := reconcileAnnotationForTest(t, instance)
	if !outcome.terminal || outcome.reason != reasonImmutableSecretConflict || outcome.desired != nil {
		t.Fatalf("immutable outcome: terminal=%t reason=%q changed=%t rotated=%t desiredNil=%t", outcome.terminal, outcome.reason, outcome.changed, outcome.rotated, outcome.desired == nil)
	}
	if !bytes.Equal(original.Data["value"], instance.Data["value"]) || len(instance.Annotations) != len(original.Annotations) {
		t.Fatal("immutable source object was mutated")
	}
}

func TestAnnotationTrackingFingerprintLossIsTerminalAndImmutableTakesPrecedence(t *testing.T) {
	setupAnnotationDefaults(t)
	created := reconcileAnnotationForTest(t, annotationTestSecret(TypeString)).desired
	created.Annotations[AnnotationGenerationFingerprint] = "{}"
	outcome := reconcileAnnotationForTest(t, created)
	if !outcome.terminal || outcome.reason != reasonTrackingStateConflict || outcome.desired != nil {
		t.Fatalf("fingerprint-loss outcome: terminal=%t reason=%q changed=%t rotated=%t desiredNil=%t", outcome.terminal, outcome.reason, outcome.changed, outcome.rotated, outcome.desired == nil)
	}

	immutable := true
	created.Immutable = &immutable
	outcome = reconcileAnnotationForTest(t, created)
	if !outcome.terminal || outcome.reason != reasonImmutableSecretConflict || outcome.desired != nil {
		t.Fatalf("immutable fingerprint-loss outcome: terminal=%t reason=%q changed=%t rotated=%t desiredNil=%t", outcome.terminal, outcome.reason, outcome.changed, outcome.rotated, outcome.desired == nil)
	}
}

func TestAnnotationInvalidRegenerateRetainedAndEventDeduplicated(t *testing.T) {
	setupAnnotationDefaults(t)
	instance := annotationTestSecret(TypeString)
	instance.Annotations[AnnotationSecretRegenerate] = "unknown"
	if _, err := buildAnnotationPlan(instance); !IsValidationError(err) {
		t.Fatalf("buildAnnotationPlan() error = %v", err)
	}
	if instance.Annotations[AnnotationSecretRegenerate] != "unknown" {
		t.Fatal("invalid regenerate request was removed")
	}

	annotationEventTransitions.Lock()
	annotationEventTransitions.last = map[string]string{}
	annotationEventTransitions.Unlock()
	recorder := record.NewFakeRecorder(2)
	r := &ReconcileSecret{recorder: recorder}
	r.recordTransition(instance, corev1.EventTypeWarning, reasonInvalidSpec, "Secret generator configuration is invalid")
	r.recordTransition(instance, corev1.EventTypeWarning, reasonInvalidSpec, "Secret generator configuration is invalid")
	select {
	case event := <-recorder.Events:
		if strings.Contains(event, "unknown") || !strings.Contains(event, reasonInvalidSpec) {
			t.Fatal("warning event exposed configuration data or omitted its safe reason")
		}
	case <-time.After(time.Second):
		t.Fatal("missing warning event")
	}
	select {
	case <-recorder.Events:
		t.Fatal("duplicate warning event")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAnnotationDeleteClearsEventDedupState(t *testing.T) {
	instance := annotationTestSecret(TypeString)
	annotationEventTransitions.Lock()
	annotationEventTransitions.last[string(instance.UID)] = reasonInvalidSpec
	annotationEventTransitions.Unlock()
	if !annotationSecretPredicate().Delete(event.TypedDeleteEvent[*corev1.Secret]{Object: instance}) {
		t.Fatal("managed annotation Secret deletion was not accepted")
	}
	annotationEventTransitions.Lock()
	_, exists := annotationEventTransitions.last[string(instance.UID)]
	annotationEventTransitions.Unlock()
	if exists {
		t.Fatal("deleted Secret retained event dedup state")
	}
}

func TestAnnotationTransitionDedupUsesUIDAcrossSameNameRecreate(t *testing.T) {
	annotationEventTransitions.Lock()
	annotationEventTransitions.last = map[string]string{}
	annotationEventTransitions.Unlock()
	recorder := record.NewFakeRecorder(2)
	r := &ReconcileSecret{recorder: recorder}
	first := annotationTestSecret(TypeString)
	first.Name, first.UID = "same-name", "first-uid"
	second := first.DeepCopy()
	second.UID = "second-uid"
	r.recordTransition(first, corev1.EventTypeWarning, reasonInvalidSpec, "Secret generator configuration is invalid")
	r.recordTransition(second, corev1.EventTypeWarning, reasonInvalidSpec, "Secret generator configuration is invalid")
	for range 2 {
		select {
		case <-recorder.Events:
		case <-time.After(time.Second):
			t.Fatal("same-name new UID transition was suppressed")
		}
	}
}

func TestAnnotationSecretPredicateUpdates(t *testing.T) {
	setupAnnotationDefaults(t)
	stable := reconcileAnnotationForTest(t, annotationTestSecret(TypeString)).desired
	stable.ResourceVersion = "1"
	predicate := annotationSecretPredicate()
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
		want   bool
	}{
		{name: "resource version only", mutate: func(s *corev1.Secret) { s.ResourceVersion = "2" }},
		{name: "unrelated annotation", mutate: func(s *corev1.Secret) { s.Annotations["example.test/note"] = "changed" }},
		{name: "configuration", mutate: func(s *corev1.Secret) { s.Annotations[AnnotationSecretLength] = "41" }, want: true},
		{name: "regeneration requested", mutate: func(s *corev1.Secret) { s.Annotations[AnnotationSecretRegenerate] = "true" }, want: true},
		{name: "regeneration removal", mutate: func(s *corev1.Secret) {
			s.Annotations[AnnotationSecretRegenerate] = "true"
			delete(s.Annotations, AnnotationSecretRegenerate)
		}},
		{name: "immutable", mutate: func(s *corev1.Secret) { immutable := true; s.Immutable = &immutable }, want: true},
		{name: "data drift", mutate: func(s *corev1.Secret) { s.Data["value"] = []byte("tampered") }, want: true},
		{name: "tracking drift", mutate: func(s *corev1.Secret) { delete(s.Annotations, AnnotationManagedDataKeys) }, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := stable.DeepCopy()
			old.Name = strings.ReplaceAll(tt.name, " ", "-")
			current := old.DeepCopy()
			tt.mutate(current)
			if got := predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: old, ObjectNew: current}); got != tt.want {
				t.Fatalf("predicate update = %v, want %v", got, tt.want)
			}
		})
	}

	old := stable.DeepCopy()
	old.Name = "controller-self-write"
	current := old.DeepCopy()
	current.ResourceVersion = "2"
	current.Data["value"] = []byte("controller-value")
	current.Annotations[AnnotationManagedDataChecksums] = `{"value":"controller-checksum"}`
	rememberAnnotationWrite(current)
	if predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: old, ObjectNew: current}) {
		t.Fatal("successful controller write was enqueued")
	}

	old = stable.DeepCopy()
	old.Name, old.UID = "same-name-new-uid", "old-uid"
	current = old.DeepCopy()
	current.ResourceVersion, current.UID = "2", "new-uid"
	current.Data["value"] = []byte("ambiguous")
	expected := current.DeepCopy()
	expected.UID = old.UID
	rememberAnnotationWrite(expected)
	if !predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: old, ObjectNew: current}) {
		t.Fatal("same-name different UID update was suppressed as a controller write")
	}
}

func TestAnnotationJointDataAndChecksumTamperIsTerminal(t *testing.T) {
	for _, immutable := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutable", true: "immutable"}[immutable], func(t *testing.T) {
			setupAnnotationDefaults(t)
			old := reconcileAnnotationForTest(t, annotationTestSecret(TypeString)).desired
			old.Name = "annotation-joint-tamper-" + map[bool]string{false: "mutable", true: "immutable"}[immutable]
			old.UID = types.UID(old.Name + "-uid")
			old.ResourceVersion = "1"
			dataOnly := old.DeepCopy()
			dataOnly.ResourceVersion = "2"
			dataOnly.Data["value"] = []byte("tampered")
			current := dataOnly.DeepCopy()
			current.ResourceVersion = "3"
			checksums := map[string]string{}
			if err := json.Unmarshal([]byte(current.Annotations[AnnotationManagedDataChecksums]), &checksums); err != nil {
				t.Fatal(err)
			}
			checksums["value"] = annotationDataChecksum("value", current.Data["value"])
			encoded, err := json.Marshal(checksums)
			if err != nil {
				t.Fatal(err)
			}
			current.Annotations[AnnotationManagedDataChecksums] = string(encoded)
			if immutable {
				value := true
				current.Immutable = &value
				dataOnly.Immutable = &value
				old.Immutable = &value
			}
			predicate := annotationSecretPredicate()
			if !predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: old, ObjectNew: dataOnly}) ||
				!predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: dataOnly, ObjectNew: current}) {
				t.Fatal("split data/checksum tamper was not enqueued")
			}
			scheme := runtime.NewScheme()
			if err = corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
			r := &ReconcileSecret{client: cl, scheme: scheme}
			_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(current)})
			if err != nil {
				t.Fatal(err)
			}
			after := &corev1.Secret{}
			if err = cl.Get(context.Background(), client.ObjectKeyFromObject(current), after); err != nil {
				t.Fatal(err)
			}
			if string(after.Data["value"]) != "tampered" {
				t.Fatal("joint tamper was silently reconciled")
			}
		})
	}
}

func TestAnnotationReturnedWriteRVResolvesAfterUnrelatedUpdate(t *testing.T) {
	setupAnnotationDefaults(t)
	base := reconcileAnnotationForTest(t, annotationTestSecret(TypeString)).desired
	base.Name, base.UID, base.ResourceVersion = "annotation-rv-race", "annotation-rv-race-uid", "1"
	drifted := base.DeepCopy()
	drifted.ResourceVersion = "2"
	drifted.Data["value"] = []byte("tampered")
	repaired := reconcileAnnotationForTest(t, drifted).desired
	repaired.ResourceVersion = "3"
	predicate := annotationSecretPredicate()
	if !predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: drifted, ObjectNew: repaired}) {
		t.Fatal("watch-before-return controller write was not enqueued")
	}
	rememberAnnotationWrite(repaired)
	later := repaired.DeepCopy()
	later.ResourceVersion = "4"
	later.Annotations["example.test/unrelated"] = "changed"
	if predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: repaired, ObjectNew: later}) {
		t.Fatal("unrelated annotation update was enqueued")
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(later).Build()
	r := &ReconcileSecret{client: cl, scheme: scheme}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(later)}); err != nil {
		t.Fatal(err)
	}
	after := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(later), after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != later.ResourceVersion || string(after.Data["value"]) != string(repaired.Data["value"]) {
		t.Fatal("late unrelated update made an exact controller write suspicious")
	}
}

func TestAnnotationProjectedSizeIncludesUnmanagedMetadata(t *testing.T) {
	setupAnnotationDefaults(t)
	instance := annotationTestSecret(TypeString)
	instance.Annotations["unmanaged-padding"] = strings.Repeat("x", MaxProjectedSecretSize)
	outcome := reconcileAnnotationForTest(t, instance)
	if !outcome.terminal || outcome.reason != reasonSecretSizeConflict || outcome.desired != nil {
		t.Fatalf("oversized outcome: terminal=%t reason=%q changed=%t rotated=%t desiredNil=%t", outcome.terminal, outcome.reason, outcome.changed, outcome.rotated, outcome.desired == nil)
	}
}

func TestValidateStartupDefaultsSSHMatrix(t *testing.T) {
	setupAnnotationDefaults(t)
	if err := ValidateStartupDefaults(); err != nil {
		t.Fatalf("valid defaults: %v", err)
	}
	viper.Set("ssh-key-length", 1024)
	if err := ValidateStartupDefaults(); !IsValidationError(err) {
		t.Fatalf("invalid RSA startup default error = %v", err)
	}
	viper.Set("ssh-key-algorithm", SSHKeyAlgorithmECDSA)
	viper.Set("ssh-key-length", 2048)
	if err := ValidateStartupDefaults(); !IsValidationError(err) {
		t.Fatalf("invalid ECDSA startup default error = %v", err)
	}
}

func setupAnnotationDefaults(t *testing.T) {
	t.Helper()
	viper.Set("secret-length", 40)
	viper.Set("secret-encoding", "base64")
	viper.Set("regenerate-insecure", false)
	viper.Set("ssh-key-length", 2048)
	viper.Set("ssh-key-algorithm", SSHKeyAlgorithmRSA)
	t.Cleanup(func() {
		viper.Set("secret-length", 40)
		viper.Set("secret-encoding", "base64")
		viper.Set("regenerate-insecure", false)
		viper.Set("ssh-key-length", 2048)
		viper.Set("ssh-key-algorithm", SSHKeyAlgorithmRSA)
	})
}
