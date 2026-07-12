package basicauth

import (
	"context"
	"fmt"
	"strconv"
	"time"

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

var log = logf.Log.WithName("controller_basicauth_secret")

const Kind = "BasicAuth"
const credentialFingerprint = "credential-set"

func Add(mgr manager.Manager) error { return add(mgr, NewReconciler(mgr)) }
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileBasicAuth{client: mgr.GetClient(), scheme: mgr.GetScheme(), recorder: mgr.GetEventRecorderFor("basicauth-controller"), clock: time.Now} //nolint:staticcheck // Event recorder API migration is deferred to M2.
}

type ReconcileBasicAuth struct {
	client   client.Client
	scheme   *runtime.Scheme
	recorder record.EventRecorder
	clock    func() time.Time
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	c, err := controller.New("basicauth-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	if err = c.Watch(source.Kind(mgr.GetCache(), &v1alpha1.BasicAuth{}, &handler.TypedEnqueueRequestForObject[*v1alpha1.BasicAuth]{}, crd.ResourceChangePredicateFor[*v1alpha1.BasicAuth](controllerobservability.ControllerBasicAuth))); err != nil {
		return err
	}
	return c.Watch(source.Kind(mgr.GetCache(), &corev1.Secret{}, handler.TypedEnqueueRequestForOwner[*corev1.Secret](mgr.GetScheme(), mgr.GetRESTMapper(), &v1alpha1.BasicAuth{}, handler.OnlyControllerOwner()), crd.SecretChangePredicateFor(controllerobservability.ControllerBasicAuth, crd.ControllerGVK(Kind))))
}

func (r *ReconcileBasicAuth) Reconcile(ctx context.Context, request reconcile.Request) (result reconcile.Result, reconcileErr error) {
	ctx, complete := controllerobservability.StartReconcile(ctx, controllerobservability.ControllerBasicAuth, r.recorder, request.NamespacedName)
	defer func() { complete(reconcileErr) }()
	logger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	instance := &v1alpha1.BasicAuth{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	controllerobservability.ObserveAPIResult(controllerobservability.ControllerBasicAuth, err)
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
		controllerobservability.ResolveControllerWrite(ctx, controllerobservability.ControllerBasicAuth, "secret_drift", existing)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	if err == nil && !crd.ExactControllerOwner(existing, instance, crd.ControllerGVK(Kind)) {
		controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretOwnershipConflict, "same-name Secret is not controlled by this BasicAuth", instance.Status.TrackingInitialized, -1)
	}
	plan, planErr := basicPlanFor(instance)
	if planErr != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonInvalidSpec, planErr.Error(), instance.Status.TrackingInitialized, -1)
	}
	if apierrors.IsNotFound(err) {
		return r.create(ctx, instance, plan, logger)
	}
	return r.update(ctx, instance, existing, plan, logger)
}

type basicPlan struct {
	constraints          secret.BasicAuthConstraints
	passwordLength       int
	fingerprint          string
	literalKeys, allKeys []string
	interval             time.Duration
}

func basicPlanFor(instance *v1alpha1.BasicAuth) (*basicPlan, error) {
	interval, err := crd.ParseRotationInterval(instance.Spec.RotationInterval)
	if err != nil {
		return nil, err
	}
	literal := make(map[string][]byte, len(instance.Spec.Data))
	for key, value := range instance.Spec.Data {
		literal[key] = []byte(value)
	}
	generated := []string{secret.FieldBasicAuthIngress, secret.FieldBasicAuthUsername, secret.FieldBasicAuthPassword}
	if err := secret.ValidateManagedFields(generated, literal); err != nil {
		return nil, err
	}
	if err := secret.ValidateBasicAuthLiteralData(literal); err != nil {
		return nil, err
	}
	username := instance.Spec.Username
	if username == "" {
		username = "admin"
	}
	encoding := instance.Spec.Encoding
	if encoding == "" {
		encoding = secret.DefaultEncoding()
	}
	cons := secret.BasicAuthConstraints{Username: username, Encoding: encoding, Length: instance.Spec.Length}
	lengths, err := secret.BasicAuthProjectedValueLengths(&cons)
	if err != nil {
		return nil, err
	}
	parsed, byteLength, err := secret.ParseByteLength(secret.DefaultLength(), instance.Spec.Length)
	if err != nil {
		return nil, err
	}
	p := &basicPlan{constraints: cons, passwordLength: lengths[secret.FieldBasicAuthPassword], literalKeys: crd.StringMapKeys(instance.Spec.Data), interval: interval}
	p.allKeys = append(append([]string(nil), p.literalKeys...), secret.FieldBasicAuthIngress, secret.FieldBasicAuthUsername, secret.FieldBasicAuthPassword)
	p.fingerprint = crd.Fingerprint([]byte(strconv.Itoa(parsed)), []byte(strconv.FormatBool(byteLength)), []byte(encoding), []byte(username))
	return p, nil
}

func (r *ReconcileBasicAuth) create(ctx context.Context, instance *v1alpha1.BasicAuth, p *basicPlan, logger logr.Logger) (reconcile.Result, error) {
	controllerobservability.ConfirmEligibleEvent(ctx, "secret_delete")
	target, err := crd.NewSecretWithScheme(instance, map[string][]byte{}, string(corev1.SecretTypeOpaque), r.scheme)
	if err != nil {
		return reconcile.Result{}, err
	}
	crd.ApplyManagedData(target, instance.Spec.Data, p.allKeys, nil)
	crd.SetRegenerationMarker(target, instance.Generation)
	if p.interval > 0 {
		crd.SetRotationAnchor(target, r.now())
	}
	t := p.tracking(instance)
	crd.ReserveChecksums(t)
	crd.StoreTracking(target, t)
	projected, _ := secret.BasicAuthProjectedValueLengths(&p.constraints)
	if err = secret.ValidateProjectedDataSize(target, projected); err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretSizeConflict, err.Error(), false, -1)
	}
	if err = secret.GenerateBasicAuthData(logger, &p.constraints, target.Data); err != nil {
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
		controllerobservability.ObserveDomainOutcome(controllerobservability.ControllerBasicAuth, "create", "success", v1alpha1.ReasonReconciled)
	}
	logger.Info("created managed Secret")
	return crd.ReadyStatusAfter(ctx, r.client, instance, target, markerOrUnset(target, instance.Generation), p.interval)
}

func (r *ReconcileBasicAuth) update(ctx context.Context, instance *v1alpha1.BasicAuth, existing *corev1.Secret, p *basicPlan, logger logr.Logger) (reconcile.Result, error) {
	if !crd.SecretTypeMatches(existing, corev1.SecretTypeOpaque) {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonSecretTypeConflict, "managed Secret type differs from the creation-time type", instance.Status.TrackingInitialized, -1)
	}
	t, state, trackingErr := crd.LoadTracking(existing)
	suspiciousTracking := controllerobservability.HasSuspiciousCandidate(ctx, "secret_drift")
	now := r.now()
	due, requeueAfter, anchored, scheduleErr := crd.RotationSchedule(existing, p.interval, now)
	if scheduleErr != nil {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonTrackingStateConflict, scheduleErr.Error(), instance.Status.TrackingInitialized, markerOrUnset(existing, instance.Generation))
	}
	if crd.IsImmutable(existing) {
		if suspiciousTracking || state != crd.TrackingValid || due || !p.immutableCurrent(instance, existing, t) {
			return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonImmutableSecretConflict, "immutable Secret requires reconciliation", instance.Status.TrackingInitialized, markerOrUnset(existing, instance.Generation))
		}
		marker, err := crd.ParseRegenerationMarker(existing, instance.Generation)
		if err != nil {
			return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonRegenerationStateConflict, err.Error(), instance.Status.TrackingInitialized, -1)
		}
		if p.interval > 0 && !anchored || p.interval == 0 && existing.Annotations[crd.AnnotationRotationAnchor] != "" {
			target := existing.DeepCopy()
			if p.interval > 0 {
				crd.SetRotationAnchor(target, now)
				requeueAfter = p.interval
			} else {
				crd.ClearRotationAnchor(target)
			}
			cc := crd.Client{Client: r.client}
			if _, err = cc.PatchSecret(ctx, existing, target); err != nil {
				return crd.ApplyFailure(ctx, r.client, instance, existing, err)
			}
			return crd.ReadyStatusAfter(ctx, r.client, instance, target, marker, requeueAfter)
		}
		return crd.ReadyStatusAfter(ctx, r.client, instance, existing, marker, requeueAfter)
	}
	if suspiciousTracking {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonTrackingStateConflict, "data and controller tracking changed outside an observed controller write", instance.Status.TrackingInitialized, markerOrUnset(existing, instance.Generation))
	}
	if state != crd.TrackingAbsent {
		if err := crd.RequireFingerprints(t, credentialFingerprint); err != nil {
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
	rotate := due || force || t.Fingerprints[credentialFingerprint] != p.fingerprint
	for _, key := range []string{secret.FieldBasicAuthIngress, secret.FieldBasicAuthUsername, secret.FieldBasicAuthPassword} {
		if t.Checksums[key] != crd.DataChecksum(key, existing.Data[key]) {
			rotate = true
		}
	}
	next := p.tracking(instance)
	if force {
		crd.SetRegenerationMarker(target, instance.Generation)
		marker = instance.Generation
	}
	if p.interval > 0 {
		if !anchored || rotate {
			crd.SetRotationAnchor(target, now)
			requeueAfter = p.interval
		}
	} else {
		crd.ClearRotationAnchor(target)
		requeueAfter = 0
	}
	crd.ReserveChecksums(next)
	crd.StoreTracking(target, next)
	if rotate {
		projected, _ := secret.BasicAuthProjectedValueLengths(&p.constraints)
		if err = secret.ValidateProjectedDataSize(target, projected); err != nil {
			return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonSecretSizeConflict, err.Error(), initialized, marker)
		}
		if err = secret.GenerateBasicAuthData(logger, &p.constraints, target.Data); err != nil {
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
		if rotate {
			controllerobservability.RecordRegenerated(ctx, instance)
		}
	}
	return crd.ReadyStatusAfter(ctx, r.client, instance, target, marker, requeueAfter)
}

func (r *ReconcileBasicAuth) now() time.Time {
	if r.clock == nil {
		return time.Now()
	}
	return r.clock()
}

func (p *basicPlan) tracking(instance *v1alpha1.BasicAuth) *crd.Tracking {
	return &crd.Tracking{DataKeys: append([]string(nil), p.allKeys...), LabelKeys: crd.StringMapKeys(instance.Labels), Checksums: map[string]string{}, Fingerprints: map[string]string{credentialFingerprint: p.fingerprint}}
}
func (p *basicPlan) validateLegacy(instance *v1alpha1.BasicAuth, existing *corev1.Secret) error {
	if !crd.SecretTypeMatches(existing, corev1.SecretTypeOpaque) {
		return fmt.Errorf("legacy Secret type does not match current spec")
	}
	for key, value := range instance.Spec.Data {
		if string(existing.Data[key]) != value {
			return fmt.Errorf("legacy literal data does not match current spec")
		}
	}
	if string(existing.Data[secret.FieldBasicAuthUsername]) != p.constraints.Username {
		return fmt.Errorf("legacy BasicAuth username does not match current spec")
	}
	parsed, bytesMode, err := secret.ParseByteLength(secret.DefaultLength(), p.constraints.Length)
	if err != nil {
		return err
	}
	if err = crd.ValidateGeneratedValue(existing.Data[secret.FieldBasicAuthPassword], parsed, bytesMode, p.constraints.Encoding); err != nil {
		return err
	}
	return secret.ValidateBasicAuthCredential(existing.Data)
}
func (p *basicPlan) immutableCurrent(instance *v1alpha1.BasicAuth, existing *corev1.Secret, t *crd.Tracking) bool {
	if t == nil || !crd.SecretTypeMatches(existing, corev1.SecretTypeOpaque) || !crd.SameStrings(t.DataKeys, p.allKeys) || !crd.SameStrings(t.LabelKeys, crd.StringMapKeys(instance.Labels)) || t.Fingerprints[credentialFingerprint] != p.fingerprint || instance.Spec.ForceRegenerate && markerOrUnset(existing, instance.Generation) < instance.Generation {
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
	for _, key := range []string{secret.FieldBasicAuthIngress, secret.FieldBasicAuthUsername, secret.FieldBasicAuthPassword} {
		if t.Checksums[key] != crd.DataChecksum(key, existing.Data[key]) {
			return false
		}
	}
	return true
}
func (r *ReconcileBasicAuth) generationFailure(ctx context.Context, instance *v1alpha1.BasicAuth, existing *corev1.Secret, err error) (reconcile.Result, error) {
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
