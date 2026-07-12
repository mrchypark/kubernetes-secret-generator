package stringsecret

import (
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

var log = logf.Log.WithName("controller_string_secret")

const Kind = "StringSecret"

func Add(mgr manager.Manager) error { return add(mgr, NewReconciler(mgr)) }
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileStringSecret{client: mgr.GetClient(), scheme: mgr.GetScheme(), recorder: mgr.GetEventRecorderFor("stringsecret-controller")} //nolint:staticcheck // Event recorder API migration is deferred to M2.
}

type ReconcileStringSecret struct {
	client   client.Client
	scheme   *runtime.Scheme
	recorder record.EventRecorder
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	c, err := controller.New("stringsecret-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}
	if err = c.Watch(source.Kind(mgr.GetCache(), &v1alpha1.StringSecret{}, &handler.TypedEnqueueRequestForObject[*v1alpha1.StringSecret]{}, crd.ResourceChangePredicateFor[*v1alpha1.StringSecret](controllerobservability.ControllerStringSecret))); err != nil {
		return err
	}
	return c.Watch(source.Kind(mgr.GetCache(), &corev1.Secret{}, handler.TypedEnqueueRequestForOwner[*corev1.Secret](mgr.GetScheme(), mgr.GetRESTMapper(), &v1alpha1.StringSecret{}, handler.OnlyControllerOwner()), crd.SecretChangePredicateFor(controllerobservability.ControllerStringSecret, crd.ControllerGVK(Kind))))
}

func (r *ReconcileStringSecret) Reconcile(ctx context.Context, request reconcile.Request) (result reconcile.Result, reconcileErr error) {
	ctx, complete := controllerobservability.StartReconcile(ctx, controllerobservability.ControllerStringSecret, r.recorder, request.NamespacedName)
	defer func() { complete(reconcileErr) }()
	logger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	instance := &v1alpha1.StringSecret{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	controllerobservability.ObserveAPIResult(controllerobservability.ControllerStringSecret, err)
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
		controllerobservability.ResolveControllerWrite(ctx, controllerobservability.ControllerStringSecret, "secret_drift", existing)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	if err == nil && !crd.ExactControllerOwner(existing, instance, crd.ControllerGVK(Kind)) {
		controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretOwnershipConflict, "same-name Secret is not controlled by this StringSecret", instance.Status.TrackingInitialized, -1)
	}
	plan, planErr := stringPlanFor(instance)
	if planErr != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonInvalidSpec, planErr.Error(), instance.Status.TrackingInitialized, -1)
	}
	if apierrors.IsNotFound(err) {
		return r.create(ctx, instance, plan, logger)
	}
	return r.update(ctx, instance, existing, plan, logger)
}

type fieldPlan struct {
	name, encoding string
	length         int
	byteLength     bool
	valueLength    int
	fingerprint    string
}

type stringPlan struct {
	fields   []fieldPlan
	dataKeys []string
	allKeys  []string
	typeName corev1.SecretType
}

func stringPlanFor(instance *v1alpha1.StringSecret) (*stringPlan, error) {
	literal := make(map[string][]byte, len(instance.Spec.Data))
	for key, value := range instance.Spec.Data {
		literal[key] = []byte(value)
	}
	names := make([]string, 0, len(instance.Spec.Fields))
	for _, field := range instance.Spec.Fields {
		names = append(names, field.FieldName)
	}
	if err := secret.ValidateManagedFields(names, literal); err != nil {
		return nil, err
	}
	p := &stringPlan{dataKeys: crd.StringMapKeys(instance.Spec.Data), typeName: crd.EffectiveSecretType(instance.Spec.Type)}
	for _, field := range instance.Spec.Fields {
		length, byteLength, err := secret.ParseByteLength(secret.DefaultLength(), field.Length)
		if err != nil {
			return nil, err
		}
		encoding := field.Encoding
		if encoding == "" {
			encoding = secret.DefaultEncoding()
		}
		if err = secret.ValidateEncoding(encoding); err != nil {
			return nil, err
		}
		valueLength, err := secret.EncodedOutputLength(length, encoding, byteLength)
		if err != nil {
			return nil, err
		}
		p.fields = append(p.fields, fieldPlan{name: field.FieldName, encoding: encoding, length: length, byteLength: byteLength, valueLength: valueLength,
			fingerprint: crd.Fingerprint([]byte(field.FieldName), []byte(strconv.Itoa(length)), []byte(strconv.FormatBool(byteLength)), []byte(encoding))})
	}
	p.allKeys = append(append([]string(nil), p.dataKeys...), names...)
	return p, nil
}

func (r *ReconcileStringSecret) create(ctx context.Context, instance *v1alpha1.StringSecret, plan *stringPlan, logger logr.Logger) (reconcile.Result, error) {
	controllerobservability.ConfirmEligibleEvent(ctx, "secret_delete")
	target, err := crd.NewSecretWithScheme(instance, map[string][]byte{}, string(plan.typeName), r.scheme)
	if err != nil {
		return reconcile.Result{}, err
	}
	tracking := plan.tracking(instance)
	crd.ApplyManagedData(target, instance.Spec.Data, plan.allKeys, nil)
	crd.SetRegenerationMarker(target, instance.Generation)
	crd.ReserveChecksums(tracking)
	crd.StoreTracking(target, tracking)
	projected := make(map[string]int, len(plan.fields))
	for _, field := range plan.fields {
		projected[field.name] = field.valueLength
	}
	if err = secret.ValidateProjectedDataSize(target, projected); err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretSizeConflict, err.Error(), false, -1)
	}
	for _, field := range plan.fields {
		target.Data[field.name], err = secret.GenerateRandomString(field.length, field.encoding, field.byteLength)
		if err != nil {
			return r.generationFailure(ctx, instance, nil, err)
		}
	}
	crd.RefreshChecksums(tracking, target.Data)
	crd.StoreTracking(target, tracking)
	if err = secret.ValidateProjectedSecretSize(target); err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, nil, v1alpha1.ReasonSecretSizeConflict, err.Error(), false, -1)
	}
	if err = r.client.Create(ctx, target); err != nil {
		return crd.ApplyFailure(ctx, r.client, instance, nil, err)
	}
	if instance.Status.Secret != nil {
		controllerobservability.RecordRecreated(ctx, instance)
	} else {
		controllerobservability.ObserveDomainOutcome(controllerobservability.ControllerStringSecret, "create", "success", v1alpha1.ReasonReconciled)
	}
	logger.Info("created managed Secret")
	return crd.ReadyStatus(ctx, r.client, instance, target, markerOrUnset(target, instance.Generation))
}

func (r *ReconcileStringSecret) update(ctx context.Context, instance *v1alpha1.StringSecret, existing *corev1.Secret, plan *stringPlan, logger logr.Logger) (reconcile.Result, error) {
	if !crd.SecretTypeMatches(existing, plan.typeName) {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonSecretTypeConflict, "managed Secret type differs from the creation-time type", instance.Status.TrackingInitialized, -1)
	}
	tracking, state, trackingErr := crd.LoadTracking(existing)
	suspiciousTracking := controllerobservability.HasSuspiciousCandidate(ctx, "secret_drift")
	if crd.IsImmutable(existing) {
		if suspiciousTracking || state != crd.TrackingValid || !plan.immutableCurrent(instance, existing, tracking) {
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
		fingerprints := make([]string, 0, len(plan.fields))
		for _, field := range plan.fields {
			fingerprints = append(fingerprints, field.name)
		}
		if err := crd.RequireFingerprints(tracking, fingerprints...); err != nil {
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
			if err = plan.validateLegacy(instance, existing); err != nil {
				return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonLegacyBaselineInvalid, err.Error(), false, marker)
			}
			tracking = plan.tracking(instance)
			crd.RefreshChecksums(tracking, existing.Data)
		} else {
			tracking = plan.tracking(instance)
		}
	}

	target := existing.DeepCopy()
	previous := []string(nil)
	if action != crd.TrackingBaseline {
		previous = tracking.DataKeys
	}
	crd.ApplyManagedData(target, instance.Spec.Data, plan.allKeys, previous)
	crd.ApplyManagedLabels(target, instance.Labels, tracking.LabelKeys)
	projected := map[string]int{}
	rotate := map[string]bool{}
	for _, field := range plan.fields {
		oldFingerprint := tracking.Fingerprints[field.name]
		checksum, checksumOK := tracking.Checksums[field.name]
		drift := !checksumOK || checksum != crd.DataChecksum(field.name, existing.Data[field.name])
		rotate[field.name] = force || state == crd.TrackingAbsent && force || oldFingerprint != field.fingerprint || drift
		if rotate[field.name] {
			projected[field.name] = field.valueLength
		}
	}
	next := plan.tracking(instance)
	if force {
		crd.SetRegenerationMarker(target, instance.Generation)
		marker = instance.Generation
	}
	crd.ReserveChecksums(next)
	crd.StoreTracking(target, next)
	if err = secret.ValidateProjectedDataSize(target, projected); err != nil {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonSecretSizeConflict, err.Error(), initialized, marker)
	}
	for _, field := range plan.fields {
		if !rotate[field.name] {
			continue
		}
		target.Data[field.name], err = secret.GenerateRandomString(field.length, field.encoding, field.byteLength)
		if err != nil {
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
		for _, rotated := range rotate {
			if rotated {
				controllerobservability.RecordRegenerated(ctx, instance)
				break
			}
		}
	}
	return crd.ReadyStatus(ctx, r.client, instance, target, marker)
}

func (p *stringPlan) tracking(instance *v1alpha1.StringSecret) *crd.Tracking {
	fp := make(map[string]string, len(p.fields))
	for _, field := range p.fields {
		fp[field.name] = field.fingerprint
	}
	return &crd.Tracking{DataKeys: append([]string(nil), p.allKeys...), LabelKeys: crd.StringMapKeys(instance.Labels), Checksums: map[string]string{}, Fingerprints: fp}
}

func (p *stringPlan) validateLegacy(instance *v1alpha1.StringSecret, existing *corev1.Secret) error {
	if !crd.SecretTypeMatches(existing, p.typeName) {
		return fmt.Errorf("legacy Secret type does not match current spec")
	}
	for key, value := range instance.Spec.Data {
		if string(existing.Data[key]) != value {
			return fmt.Errorf("legacy literal data does not match current spec")
		}
	}
	for _, field := range p.fields {
		if err := crd.ValidateGeneratedValue(existing.Data[field.name], field.length, field.byteLength, field.encoding); err != nil {
			return err
		}
	}
	return nil
}

func (p *stringPlan) immutableCurrent(instance *v1alpha1.StringSecret, existing *corev1.Secret, t *crd.Tracking) bool {
	if t == nil || !crd.SecretTypeMatches(existing, p.typeName) || !crd.SameStrings(t.DataKeys, p.allKeys) || !crd.SameStrings(t.LabelKeys, crd.StringMapKeys(instance.Labels)) || instance.Spec.ForceRegenerate && markerOrUnset(existing, instance.Generation) < instance.Generation {
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
	for _, field := range p.fields {
		if t.Fingerprints[field.name] != field.fingerprint || t.Checksums[field.name] != crd.DataChecksum(field.name, existing.Data[field.name]) {
			return false
		}
	}
	return true
}

func (r *ReconcileStringSecret) generationFailure(ctx context.Context, instance *v1alpha1.StringSecret, existing *corev1.Secret, err error) (reconcile.Result, error) {
	if secret.IsValidationError(err) {
		return crd.TerminalStatus(ctx, r.client, instance, existing, v1alpha1.ReasonInvalidSpec, err.Error(), false, -1)
	}
	cc := crd.Client{Client: r.client}
	_ = cc.SetStatus(ctx, instance, existing, metav1.ConditionFalse, v1alpha1.ReasonGenerationFailed, "credential generation failed", instance.Status.TrackingInitialized, -1)
	return reconcile.Result{}, err
}

func markerOrUnset(secret *corev1.Secret, generation int64) int64 {
	marker, err := crd.ParseRegenerationMarker(secret, generation)
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
