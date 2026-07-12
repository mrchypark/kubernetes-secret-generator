package sshkeypair

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
	controllerobservability "github.com/mittwald/kubernetes-secret-generator/pkg/controller/observability"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
)

var log = logf.Log.WithName("controller_ssh_secret")

const Kind = "SSHKeyPair"
const keyFingerprint = "key-set"

func Add(mgr manager.Manager) error { return add(mgr, NewReconciler(mgr)) }
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileSSHKeyPair{client: mgr.GetClient(), scheme: mgr.GetScheme(), recorder: mgr.GetEventRecorderFor("sshkeypair-controller")} //nolint:staticcheck // Event recorder API migration is deferred to M2.
}

type ReconcileSSHKeyPair struct {
	client   client.Client
	scheme   *runtime.Scheme
	recorder record.EventRecorder
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	c, err := controller.New("ssh-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	if err = c.Watch(source.Kind(mgr.GetCache(), &v1alpha1.SSHKeyPair{}, &handler.TypedEnqueueRequestForObject[*v1alpha1.SSHKeyPair]{}, crd.ResourceChangePredicateFor[*v1alpha1.SSHKeyPair](controllerobservability.ControllerSSHKeyPair))); err != nil {
		return err
	}
	return c.Watch(source.Kind(mgr.GetCache(), &corev1.Secret{}, handler.TypedEnqueueRequestForOwner[*corev1.Secret](mgr.GetScheme(), mgr.GetRESTMapper(), &v1alpha1.SSHKeyPair{}, handler.OnlyControllerOwner()), crd.SecretChangePredicateFor(controllerobservability.ControllerSSHKeyPair, crd.ControllerGVK(Kind))))
}

func (r *ReconcileSSHKeyPair) Reconcile(ctx context.Context, request reconcile.Request) (result reconcile.Result, reconcileErr error) {
	ctx, complete := controllerobservability.StartReconcile(ctx, controllerobservability.ControllerSSHKeyPair, r.recorder, request.NamespacedName)
	defer func() { complete(reconcileErr) }()
	logger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	instance := &v1alpha1.SSHKeyPair{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	controllerobservability.ObserveAPIResult(controllerobservability.ControllerSSHKeyPair, err)
	if err != nil {
		return crd.CheckError(err)
	}
	controllerobservability.BindReconcileObject(ctx, instance)
	existing := &corev1.Secret{}
	err = r.client.Get(ctx, request.NamespacedName, existing)
	if apierrors.IsNotFound(err) {
		controllerobservability.BindReconcileDependentMissing(ctx, request.Name)
	} else if err == nil {
		controllerobservability.BindReconcileDependent(ctx, existing)
		controllerobservability.ResolveControllerWrite(ctx, controllerobservability.ControllerSSHKeyPair, "secret_drift", existing)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	if err == nil && !crd.ExactControllerOwner(existing, instance, crd.ControllerGVK(Kind)) {
		controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretOwnershipConflict, "same-name Secret is not controlled by this SSHKeyPair", instance.Status.TrackingInitialized, -1)
	}
	plan, planErr := sshPlanFor(instance)
	if planErr != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonInvalidSpec, planErr.Error(), instance.Status.TrackingInitialized, -1)
	}
	if apierrors.IsNotFound(err) {
		return r.create(ctx, instance, plan, logger)
	}
	return r.update(ctx, instance, existing, plan, logger)
}

type sshPlan struct {
	algorithm, length, privateField, publicField, supplied string
	expectedLength                                         int
	fingerprint                                            string
	typeName                                               corev1.SecretType
	literalKeys, allKeys                                   []string
}

func sshPlanFor(instance *v1alpha1.SSHKeyPair) (*sshPlan, error) {
	literal := make(map[string][]byte, len(instance.Spec.Data))
	for key, value := range instance.Spec.Data {
		literal[key] = []byte(value)
	}
	privateField, publicField := instance.GetPrivateKeyField(), instance.GetPublicKeyField()
	if err := secret.ValidateSSHFields(privateField, publicField, literal); err != nil {
		return nil, err
	}
	if err := secret.ValidateManagedFields([]string{privateField, publicField}, literal); err != nil {
		return nil, err
	}
	algorithm, expected, err := secret.ValidateSSHConfiguration(instance.Spec.Algorithm, instance.Spec.Length)
	if err != nil {
		return nil, err
	}
	length := instance.Spec.Length
	if algorithm == secret.SSHKeyAlgorithmED25519 {
		length = ""
	} else if length == "" {
		length = strconv.Itoa(expected)
	}
	if instance.Spec.PrivateKey != "" {
		if _, err = secret.ValidateSSHPrivateKey([]byte(instance.Spec.PrivateKey), algorithm, expected); err != nil {
			return nil, err
		}
	}
	typeName := crd.EffectiveSecretType(instance.Spec.Type)
	p := &sshPlan{algorithm: algorithm, length: length, expectedLength: expected, privateField: privateField, publicField: publicField, supplied: instance.Spec.PrivateKey, typeName: typeName, literalKeys: crd.StringMapKeys(instance.Spec.Data)}
	p.allKeys = append(append([]string(nil), p.literalKeys...), privateField, publicField)
	p.fingerprint = crd.Fingerprint([]byte(algorithm), []byte(strconv.Itoa(expected)), []byte(privateField), []byte(publicField), []byte(instance.Spec.PrivateKey))
	return p, nil
}

func (r *ReconcileSSHKeyPair) create(ctx context.Context, instance *v1alpha1.SSHKeyPair, p *sshPlan, logger logr.Logger) (reconcile.Result, error) {
	controllerobservability.ConfirmEligibleEvent(ctx, "secret_delete")
	target, err := crd.NewSecretWithScheme(instance, map[string][]byte{}, string(p.typeName), r.scheme)
	if err != nil {
		return reconcile.Result{}, err
	}
	crd.ApplyManagedData(target, instance.Spec.Data, p.allKeys, nil)
	if p.supplied != "" {
		target.Data[p.privateField] = []byte(p.supplied)
	}
	crd.SetRegenerationMarker(target, instance.Generation)
	t := p.tracking(instance)
	crd.ReserveChecksums(t)
	crd.StoreTracking(target, t)
	projected, err := secret.SSHProjectedValueLengths(p.algorithm, p.expectedLength, p.privateField, p.publicField, false, target.Data)
	if err != nil {
		return r.generationFailure(ctx, instance, nil, err)
	}
	if err = secret.ValidateProjectedDataSize(target, projected); err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretSizeConflict, err.Error(), false, -1)
	}
	if err = secret.GenerateSSHKeypairDataWithAlgorithm(logger, p.algorithm, p.length, p.privateField, p.publicField, false, target.Data); err != nil {
		return r.generationFailure(ctx, instance, nil, err)
	}
	crd.RefreshChecksums(t, target.Data)
	crd.StoreTracking(target, t)
	if err = secret.ValidateProjectedSecretSize(target); err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretSizeConflict, err.Error(), false, -1)
	}
	if err = r.client.Create(ctx, target); err != nil {
		return crd.ApplyFailure(ctx, r.client, instance, nil, err)
	}
	if instance.Status.Secret != nil {
		controllerobservability.RecordRecreated(ctx, instance)
	} else {
		controllerobservability.ObserveDomainOutcome(controllerobservability.ControllerSSHKeyPair, "create", "success", v1alpha1.ReasonReconciled)
	}
	logger.Info("created managed Secret")
	return crd.ReadyStatus(ctx, r.client, instance, target, markerOrUnset(target, instance.Generation))
}

func (r *ReconcileSSHKeyPair) update(ctx context.Context, instance *v1alpha1.SSHKeyPair, existing *corev1.Secret, p *sshPlan, logger logr.Logger) (reconcile.Result, error) {
	if !crd.SecretTypeMatches(existing, p.typeName) {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonSecretTypeConflict, "managed Secret type differs from the creation-time type", instance.Status.TrackingInitialized, -1)
	}
	t, state, trackingErr := crd.LoadTracking(existing)
	suspiciousTracking := controllerobservability.HasSuspiciousCandidate(ctx, "secret_drift")
	if crd.IsImmutable(existing) {
		if suspiciousTracking || state != crd.TrackingValid || !p.immutableCurrent(instance, existing, t) {
			return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonImmutableSecretConflict, "immutable Secret requires reconciliation", instance.Status.TrackingInitialized, markerOrUnset(existing, instance.Generation))
		}
		marker, err := crd.ParseRegenerationMarker(existing, instance.Generation)
		if err != nil {
			return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonRegenerationStateConflict, err.Error(), instance.Status.TrackingInitialized, -1)
		}
		return crd.ReadyStatus(ctx, r.client, instance, existing, marker)
	}
	if suspiciousTracking {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonTrackingStateConflict, "data and controller tracking changed outside an observed controller write", instance.Status.TrackingInitialized, markerOrUnset(existing, instance.Generation))
	}
	if state != crd.TrackingAbsent {
		if err := crd.RequireFingerprints(t, keyFingerprint); err != nil {
			state, trackingErr = crd.TrackingConflict, err
		}
	}
	initialized := instance.Status.TrackingInitialized
	action := crd.ClassifyTracking(initialized, state)
	if action == crd.TrackingReject {
		if trackingErr == nil {
			trackingErr = fmt.Errorf("tracking bundle is absent after initialization")
		}
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonTrackingStateConflict, trackingErr.Error(), initialized, markerOrUnset(existing, instance.Generation))
	}
	marker, err := crd.ParseRegenerationMarker(existing, instance.Generation)
	if err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonRegenerationStateConflict, err.Error(), initialized, -1)
	}
	force := instance.Spec.ForceRegenerate && marker < instance.Generation
	if action == crd.TrackingBaseline {
		if !force {
			if err = p.validateLegacy(instance, existing); err != nil {
				return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonLegacyBaselineInvalid, err.Error(), false, marker)
			}
			t = p.tracking(instance)
			crd.RefreshChecksums(t, existing.Data)
		} else {
			t = p.tracking(instance)
		}
	}
	target := existing.DeepCopy()
	previous := []string(nil)
	if action != crd.TrackingBaseline {
		previous = t.DataKeys
	}
	crd.ApplyManagedData(target, instance.Spec.Data, p.allKeys, previous)
	crd.ApplyManagedLabels(target, instance.Labels, t.LabelKeys)
	fingerprintChanged := t.Fingerprints[keyFingerprint] != p.fingerprint
	privateDrift := t.Checksums[p.privateField] != crd.DataChecksum(p.privateField, existing.Data[p.privateField])
	publicDrift := t.Checksums[p.publicField] != crd.DataChecksum(p.publicField, existing.Data[p.publicField])
	rotatePair := force || fingerprintChanged || privateDrift
	if p.supplied != "" && rotatePair {
		target.Data[p.privateField] = []byte(p.supplied)
		rotatePair = false
		publicDrift = true
	}
	if rotatePair {
		delete(target.Data, p.privateField)
		delete(target.Data, p.publicField)
	}
	next := p.tracking(instance)
	if force {
		crd.SetRegenerationMarker(target, instance.Generation)
		marker = instance.Generation
	}
	crd.ReserveChecksums(next)
	crd.StoreTracking(target, next)
	if rotatePair || publicDrift {
		projected, projectErr := secret.SSHProjectedValueLengths(p.algorithm, p.expectedLength, p.privateField, p.publicField, rotatePair, target.Data)
		if projectErr != nil {
			return r.generationFailure(ctx, instance, existing, projectErr)
		}
		if err = secret.ValidateProjectedDataSize(target, projected); err != nil {
			return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonSecretSizeConflict, err.Error(), initialized, marker)
		}
		if err = secret.GenerateSSHKeypairDataWithAlgorithm(logger, p.algorithm, p.length, p.privateField, p.publicField, rotatePair, target.Data); err != nil {
			return r.generationFailure(ctx, instance, existing, err)
		}
	}
	crd.RefreshChecksums(next, target.Data)
	crd.StoreTracking(target, next)
	if err = secret.ValidateProjectedSecretSize(target); err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonSecretSizeConflict, err.Error(), initialized, marker)
	}
	cc := crd.Client{Client: r.client}
	wrote, err := cc.PatchSecret(ctx, existing, target)
	if err != nil {
		return crd.ApplyFailure(ctx, r.client, instance, existing, err)
	}
	if wrote {
		logger.Info("reconciled managed Secret")
		if rotatePair {
			controllerobservability.RecordRegenerated(ctx, instance)
		}
	}
	return crd.ReadyStatus(ctx, r.client, instance, target, marker)
}

func (p *sshPlan) tracking(instance *v1alpha1.SSHKeyPair) *crd.Tracking {
	return &crd.Tracking{DataKeys: append([]string(nil), p.allKeys...), LabelKeys: crd.StringMapKeys(instance.Labels), Checksums: map[string]string{}, Fingerprints: map[string]string{keyFingerprint: p.fingerprint}}
}
func (p *sshPlan) validateLegacy(instance *v1alpha1.SSHKeyPair, existing *corev1.Secret) error {
	if !crd.SecretTypeMatches(existing, p.typeName) {
		return fmt.Errorf("legacy Secret type does not match current spec")
	}
	for key, value := range instance.Spec.Data {
		if string(existing.Data[key]) != value {
			return fmt.Errorf("legacy literal data does not match current spec")
		}
	}
	if p.supplied != "" && !bytes.Equal(existing.Data[p.privateField], []byte(p.supplied)) {
		return fmt.Errorf("legacy supplied private key does not match current spec")
	}
	key, err := secret.ValidateSSHPrivateKey(existing.Data[p.privateField], p.algorithm, p.expectedLength)
	if err != nil {
		return err
	}
	want, err := secret.SSHPublicKeyForPrivateKey(key)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, existing.Data[p.publicField]) {
		return fmt.Errorf("legacy SSH public key does not match private key")
	}
	return nil
}
func (p *sshPlan) immutableCurrent(instance *v1alpha1.SSHKeyPair, existing *corev1.Secret, t *crd.Tracking) bool {
	if t == nil || !crd.SecretTypeMatches(existing, p.typeName) || !crd.SameStrings(t.DataKeys, p.allKeys) || !crd.SameStrings(t.LabelKeys, crd.StringMapKeys(instance.Labels)) || t.Fingerprints[keyFingerprint] != p.fingerprint || instance.Spec.ForceRegenerate && markerOrUnset(existing, instance.Generation) < instance.Generation {
		return false
	}
	if !sameManagedLabels(existing.Labels, instance.Labels, t.LabelKeys) {
		return false
	}
	for key, value := range instance.Spec.Data {
		if string(existing.Data[key]) != value {
			return false
		}
	}
	for _, key := range []string{p.privateField, p.publicField} {
		if t.Checksums[key] != crd.DataChecksum(key, existing.Data[key]) {
			return false
		}
	}
	return true
}
func (r *ReconcileSSHKeyPair) generationFailure(ctx context.Context, instance *v1alpha1.SSHKeyPair, existing *corev1.Secret, err error) (reconcile.Result, error) {
	if secret.IsValidationError(err) {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonInvalidSpec, err.Error(), false, -1)
	}
	cc := crd.Client{Client: r.client}
	_ = cc.SetStatus(ctx, instance, existing, metav1.ConditionFalse, v1alpha1.ReasonGenerationFailed, "credential generation failed", instance.Status.TrackingInitialized, -1)
	return reconcile.Result{}, err
}
func markerOrUnset(s *corev1.Secret, generation int64) int64 {
	marker, err := crd.ParseRegenerationMarker(s, generation)
	if err != nil {
		return -1
	}
	return marker
}
func sameManagedLabels(have, want map[string]string, previous []string) bool {
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	for _, key := range previous {
		if _, ok := want[key]; !ok {
			if _, exists := have[key]; exists {
				return false
			}
		}
	}
	return true
}
