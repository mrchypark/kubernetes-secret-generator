package secret

import (
	"context"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	controllerobservability "github.com/mittwald/kubernetes-secret-generator/pkg/controller/observability"
)

const ByteSuffix = "b"

var log = logf.Log.WithName("controller_secret")

func RegenerateInsecure() bool {
	return viper.GetBool("regenerate-insecure")
}

func DefaultLength() int {
	return viper.GetInt("secret-length")
}

func DefaultEncoding() string {
	return viper.GetString("secret-encoding")
}

func SSHKeyLength() int {
	return viper.GetInt("ssh-key-length")
}

func SSHKeyAlgorithm() string {
	algorithm := viper.GetString("ssh-key-algorithm")
	if algorithm == "" {
		return SSHKeyAlgorithmRSA
	}
	return algorithm
}

// ValidateStartupDefaults validates manager-provided defaults before any
// controller starts or cryptographic operation can be reached.
func ValidateStartupDefaults() error {
	if _, _, err := ParseByteLength(DefaultLength(), strconv.Itoa(DefaultLength())); err != nil {
		return err
	}
	if err := ValidateEncoding(DefaultEncoding()); err != nil {
		return err
	}
	_, _, err := ValidateSSHConfiguration(SSHKeyAlgorithm(), strconv.Itoa(SSHKeyLength()))
	return err
}

// Add creates a new Secret Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, NewReconciler(mgr))
}

// NewReconciler returns a new reconcile.Reconciler
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileSecret{client: mgr.GetClient(), scheme: mgr.GetScheme(), recorder: mgr.GetEventRecorderFor("secret-controller")} //nolint:staticcheck // Event recorder API migration is deferred to M2.
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("secret-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Secret
	err = c.Watch(source.Kind(mgr.GetCache(), &corev1.Secret{}, &handler.TypedEnqueueRequestForObject[*corev1.Secret]{}, annotationSecretPredicate()))
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileSecret implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileSecret{}

// ReconcileSecret reconciles a Secret object
type ReconcileSecret struct {
	// This Client, initialized using mgr.Client() above, is a split Client
	// that reads objects from the cache and writes to the apiserver
	client   client.Client
	scheme   *runtime.Scheme
	recorder record.EventRecorder
}

var annotationEventTransitions = struct {
	sync.Mutex
	last map[string]string
}{last: map[string]string{}}

var annotationExpectedWrites = struct {
	sync.Mutex
	resourceVersions map[string]annotationExpectedWrite
}{resourceVersions: map[string]annotationExpectedWrite{}}

type annotationExpectedWrite struct {
	uid             types.UID
	resourceVersion string
	expires         time.Time
}

const (
	annotationWriteTTL          = time.Minute
	maxAnnotationTransitionKeys = 1024
)

// Reconcile reads that state of the cluster for a Secret object and makes changes based on the state read
// and what is in the Secret.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileSecret) Reconcile(ctx context.Context, request reconcile.Request) (result reconcile.Result, reconcileErr error) {
	ctx, complete := controllerobservability.StartReconcile(ctx, controllerobservability.ControllerAnnotation, r.recorder, request.NamespacedName)
	defer func() { complete(reconcileErr) }()
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Secret")

	// Fetch the Secret instance
	instance := &corev1.Secret{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	controllerobservability.ObserveAPIResult(controllerobservability.ControllerAnnotation, err)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			clearAnnotationTransition(request.Namespace + "/" + request.Name)
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	controllerobservability.BindReconcileObject(ctx, instance)
	if controllerobservability.HasSuspiciousCandidate(ctx, "secret_drift") {
		resolveAnnotationWrite(ctx, instance)
	}

	plan, err := buildAnnotationPlan(instance)
	if err != nil {
		if IsValidationError(err) {
			controllerobservability.SetConditionOutcome(ctx, metav1.ConditionFalse, reasonInvalidSpec)
			r.recordTransition(instance, corev1.EventTypeWarning, reasonInvalidSpec, "Secret generator configuration is invalid")
			reqLogger.Info("terminal reconciliation", "reason", reasonInvalidSpec)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if plan == nil {
		r.markTransition(instance, reasonReconciled)
		return reconcile.Result{}, nil
	}
	if controllerobservability.HasSuspiciousCandidate(ctx, "secret_drift") {
		reason := reasonTrackingStateConflict
		if instance.Immutable != nil && *instance.Immutable {
			reason = reasonImmutableSecretConflict
		}
		controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
		controllerobservability.SetConditionOutcome(ctx, metav1.ConditionFalse, reason)
		r.recordTransition(instance, corev1.EventTypeWarning, reason, annotationTerminalMessage(reason))
		reqLogger.Info("terminal reconciliation", "reason", reason)
		return reconcile.Result{}, nil
	}

	outcome, err := reconcileAnnotationSecret(instance, plan)
	if err != nil {
		controllerobservability.SetConditionOutcome(ctx, metav1.ConditionFalse, reasonGenerationFailed)
		r.recordTransition(instance, corev1.EventTypeWarning, reasonGenerationFailed, "Secret generation failed")
		reqLogger.Error(err, "reconciliation failed", "reason", reasonGenerationFailed)
		return reconcile.Result{}, err
	}
	if outcome.terminal {
		controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
		controllerobservability.SetConditionOutcome(ctx, metav1.ConditionFalse, outcome.reason)
		r.recordTransition(instance, corev1.EventTypeWarning, outcome.reason, annotationTerminalMessage(outcome.reason))
		reqLogger.Info("terminal reconciliation", "reason", outcome.reason)
		return reconcile.Result{}, nil
	}
	if !outcome.changed {
		controllerobservability.SetConditionOutcome(ctx, metav1.ConditionTrue, reasonReconciled)
		r.markTransition(instance, reasonReconciled)
		return reconcile.Result{}, nil
	}
	controllerobservability.ConfirmEligibleEvent(ctx, "secret_drift")
	if err := r.client.Update(ctx, outcome.desired); err != nil {
		controllerobservability.SetConditionOutcome(ctx, metav1.ConditionFalse, "ApplyFailed")
		r.recordTransition(instance, corev1.EventTypeWarning, "ApplyFailed", "Secret reconciliation could not be applied")
		reqLogger.Error(err, "reconciliation failed", "reason", "ApplyFailed")
		return reconcile.Result{}, err
	}
	rememberAnnotationWrite(outcome.desired)
	controllerobservability.SetConditionOutcome(ctx, metav1.ConditionTrue, reasonReconciled)
	if outcome.rotated {
		r.markTransition(instance, reasonReconciled)
		controllerobservability.RecordRegenerated(ctx, instance)
	} else {
		r.recordSuccess(instance, annotationSuccessMessage(false))
	}

	return reconcile.Result{}, nil
}

func annotationSecretPredicate() predicate.TypedPredicate[*corev1.Secret] {
	return predicate.TypedFuncs[*corev1.Secret]{
		CreateFunc: func(e event.TypedCreateEvent[*corev1.Secret]) bool {
			managed := isAnnotationManaged(e.Object)
			if managed {
				controllerobservability.ObserveEligibleEvent(controllerobservability.ControllerAnnotation, "resource_create", e.Object)
			}
			return managed
		},
		UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Secret]) bool {
			managed := isAnnotationManaged(e.ObjectOld) || isAnnotationManaged(e.ObjectNew)
			if !managed {
				return false
			}
			if consumeAnnotationWrite(e.ObjectNew) {
				return false
			}
			if annotationConfigurationChanged(e.ObjectOld, e.ObjectNew) {
				controllerobservability.ObserveEligibleEvent(controllerobservability.ControllerAnnotation, "resource_update", e.ObjectNew)
				return true
			}
			if annotationExternalStateChanged(e.ObjectOld, e.ObjectNew) {
				dataChanged := !reflect.DeepEqual(e.ObjectOld.Data, e.ObjectNew.Data)
				trackingChanged := !reflect.DeepEqual(annotationTrackingConfiguration(e.ObjectOld), annotationTrackingConfiguration(e.ObjectNew))
				// Annotation-only objects have no independent cold-start checksum
				// authority; observed valid tracking advances are therefore fail-closed.
				_, state, _ := loadAnnotationTracking(e.ObjectNew)
				validTrackingAdvance := trackingChanged && state == annotationTrackingValid
				controllerobservability.ObserveEligibleCandidateChanges(controllerobservability.ControllerAnnotation, "secret_drift", e.ObjectNew, dataChanged || validTrackingAdvance, trackingChanged)
				return true
			}
			return false
		},
		// Enqueue deletion only to release transition-dedup state. There is no
		// remaining annotation source from which data could be recreated.
		DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Secret]) bool {
			managed := isAnnotationManaged(e.Object)
			if managed {
				annotationEventTransitions.Lock()
				delete(annotationEventTransitions.last, string(e.Object.UID))
				annotationEventTransitions.Unlock()
				forgetAnnotationWrite(e.Object)
			}
			return managed
		},
	}
}

func isAnnotationManaged(secret *corev1.Secret) bool {
	if secret == nil {
		return false
	}
	_, auto := secret.Annotations[AnnotationSecretAutoGenerate]
	return auto || Type(secret.Annotations[AnnotationSecretType]) == TypeString || Type(secret.Annotations[AnnotationSecretType]) == TypeBasicAuth || Type(secret.Annotations[AnnotationSecretType]) == TypeSSHKeypair
}

func annotationConfigurationChanged(old, current *corev1.Secret) bool {
	if !equalAnnotationBool(old.Immutable, current.Immutable) || !reflect.DeepEqual(annotationSourceConfiguration(old), annotationSourceConfiguration(current)) {
		return true
	}
	oldRegenerate, oldRequested := old.Annotations[AnnotationSecretRegenerate]
	newRegenerate, newRequested := current.Annotations[AnnotationSecretRegenerate]
	// Successful reconciliation removes the request and must not add its own
	// write to the eligible-event denominator.
	return newRequested && (!oldRequested || oldRegenerate != newRegenerate)
}

func annotationSourceConfiguration(secret *corev1.Secret) map[string]string {
	keys := []string{
		AnnotationSecretAutoGenerate, AnnotationSecretType, AnnotationSecretLength,
		AnnotationSSHKeyAlgorithm, AnnotationBasicAuthUsername, AnnotationSecretEncoding,
		AnnotationSSHPrivateKeyField, AnnotationSSHPublicKeyField,
	}
	configuration := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := secret.Annotations[key]; ok {
			configuration[key] = value
		}
	}
	if configuration[AnnotationSecretType] == "" {
		if _, ok := configuration[AnnotationSecretAutoGenerate]; ok {
			configuration[AnnotationSecretType] = string(TypeString)
		}
	}
	return configuration
}

func annotationExternalStateChanged(old, current *corev1.Secret) bool {
	return !reflect.DeepEqual(old.Data, current.Data) ||
		!reflect.DeepEqual(annotationTrackingConfiguration(old), annotationTrackingConfiguration(current))
}

func annotationTrackingConfiguration(secret *corev1.Secret) map[string]string {
	configuration := make(map[string]string, len(annotationTrackingKeys))
	for _, key := range annotationTrackingKeys {
		if value, ok := secret.Annotations[key]; ok {
			configuration[key] = value
		}
	}
	return configuration
}

func annotationObjectKey(secret *corev1.Secret) string {
	return secret.Namespace + "/" + secret.Name
}

func rememberAnnotationWrite(secret *corev1.Secret) {
	if secret.ResourceVersion == "" {
		return
	}
	annotationExpectedWrites.Lock()
	annotationExpectedWrites.resourceVersions[annotationObjectKey(secret)] = annotationExpectedWrite{
		uid: secret.UID, resourceVersion: secret.ResourceVersion, expires: time.Now().Add(annotationWriteTTL),
	}
	annotationExpectedWrites.Unlock()
}

func consumeAnnotationWrite(secret *corev1.Secret) bool {
	key := annotationObjectKey(secret)
	annotationExpectedWrites.Lock()
	expected, ok := annotationExpectedWrites.resourceVersions[key]
	if !ok || time.Now().After(expected.expires) || expected.uid != secret.UID {
		delete(annotationExpectedWrites.resourceVersions, key)
	} else if expected.resourceVersion == secret.ResourceVersion {
		delete(annotationExpectedWrites.resourceVersions, key)
	}
	annotationExpectedWrites.Unlock()
	return ok && time.Now().Before(expected.expires) && expected.uid == secret.UID && expected.resourceVersion == secret.ResourceVersion
}

func resolveAnnotationWrite(ctx context.Context, secret *corev1.Secret) bool {
	key := annotationObjectKey(secret)
	annotationExpectedWrites.Lock()
	expected, ok := annotationExpectedWrites.resourceVersions[key]
	annotationExpectedWrites.Unlock()
	if !ok || time.Now().After(expected.expires) || expected.uid != secret.UID ||
		!controllerobservability.ResolveCandidateWrite(ctx, "secret_drift", expected.uid, expected.resourceVersion) {
		return false
	}
	annotationExpectedWrites.Lock()
	delete(annotationExpectedWrites.resourceVersions, key)
	annotationExpectedWrites.Unlock()
	return true
}

func forgetAnnotationWrite(secret *corev1.Secret) {
	annotationExpectedWrites.Lock()
	delete(annotationExpectedWrites.resourceVersions, annotationObjectKey(secret))
	annotationExpectedWrites.Unlock()
}

func equalAnnotationBool(a, b *bool) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (r *ReconcileSecret) recordTransition(secret *corev1.Secret, eventType, reason, message string) {
	key := annotationTransitionKey(secret)
	annotationEventTransitions.Lock()
	if annotationEventTransitions.last[key] == reason {
		annotationEventTransitions.Unlock()
		return
	}
	if _, exists := annotationEventTransitions.last[key]; !exists && len(annotationEventTransitions.last) >= maxAnnotationTransitionKeys {
		for oldest := range annotationEventTransitions.last {
			delete(annotationEventTransitions.last, oldest)
			break
		}
	}
	annotationEventTransitions.last[key] = reason
	annotationEventTransitions.Unlock()
	if reason != reasonReconciled {
		controllerobservability.ObserveDomainOutcome(controllerobservability.ControllerAnnotation, "condition", "terminal", reason)
	}
	if r.recorder != nil {
		r.recorder.Event(secret, eventType, reason, message)
	}
}

func (r *ReconcileSecret) markTransition(secret *corev1.Secret, reason string) {
	key := annotationTransitionKey(secret)
	annotationEventTransitions.Lock()
	if _, exists := annotationEventTransitions.last[key]; !exists && len(annotationEventTransitions.last) >= maxAnnotationTransitionKeys {
		for oldest := range annotationEventTransitions.last {
			delete(annotationEventTransitions.last, oldest)
			break
		}
	}
	annotationEventTransitions.last[key] = reason
	annotationEventTransitions.Unlock()
}

func annotationTransitionKey(secret *corev1.Secret) string {
	if secret.UID != "" {
		return string(secret.UID)
	}
	return annotationObjectKey(secret)
}

func clearAnnotationTransition(key string) {
	annotationEventTransitions.Lock()
	delete(annotationEventTransitions.last, key)
	annotationEventTransitions.Unlock()
}

func (r *ReconcileSecret) recordSuccess(secret *corev1.Secret, message string) {
	r.markTransition(secret, reasonReconciled)
	if r.recorder != nil {
		r.recorder.Event(secret, corev1.EventTypeNormal, reasonReconciled, message)
	}
}

func annotationTerminalMessage(reason string) string {
	switch reason {
	case reasonLegacyBaselineInvalid:
		return "Existing generated data does not match the effective configuration"
	case reasonTrackingStateConflict:
		return "Secret generator tracking state is inconsistent"
	case reasonSecretSizeConflict:
		return "Projected Secret size exceeds the supported limit"
	case reasonImmutableSecretConflict:
		return "Immutable Secret requires reconciliation and was left unchanged"
	default:
		return "Secret generator configuration is invalid"
	}
}

func annotationSuccessMessage(rotated bool) string {
	if rotated {
		return "Generated Secret data was reconciled"
	}
	return "Generated Secret state was reconciled"
}

func GetLengthFromAnnotation(fallback int, annotations map[string]string) (string, error) {
	l := fallback

	if val, ok := annotations[AnnotationSecretLength]; ok {
		return val, nil
	}
	return strconv.Itoa(l), nil
}

func getEncodingFromAnnotation(fallback string, annotations map[string]string) (string, error) {
	encoding := fallback
	if val, ok := annotations[AnnotationSecretEncoding]; ok {
		encoding = val
	}
	if err := ValidateEncoding(encoding); err != nil {
		return "", err
	}
	return encoding, nil
}

func GetPrivateKeyFieldFromAnnotation(fallback string, annotations map[string]string) (string, error) {
	field := fallback
	if val, ok := annotations[AnnotationSSHPrivateKeyField]; ok {
		field = val
	}
	if err := ValidateFieldName(field); err != nil {
		return "", err
	}
	return field, nil
}

func GetPublicKeyFieldFromAnnotation(fallback string, annotations map[string]string) (string, error) {
	field := fallback
	if val, ok := annotations[AnnotationSSHPublicKeyField]; ok {
		field = val
	}
	if err := ValidateFieldName(field); err != nil {
		return "", err
	}
	return field, nil
}
