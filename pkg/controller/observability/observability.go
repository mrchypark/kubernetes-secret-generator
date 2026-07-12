package observability

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	ControllerStringSecret = "stringsecret"
	ControllerBasicAuth    = "basicauth"
	ControllerSSHKeyPair   = "sshkeypair"
	ControllerAnnotation   = "annotation"
	ControllerManager      = "manager"

	EventReasonSecretRecreated = "SecretRecreated"
	EventReasonRegenerated     = "SecretRegenerated"
)

var (
	domainOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubernetes_secret_generator",
		Name:      "domain_outcomes_total",
		Help:      "Low-cardinality domain outcomes observed by custom resource controllers.",
	}, []string{"controller", "operation", "outcome", "reason"})
	eligibleEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubernetes_secret_generator",
		Name:      "eligible_events_total",
		Help:      "Eligible reconciliation work items observed by custom resource controllers.",
	}, []string{"controller", "source"})
	completionLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "kubernetes_secret_generator",
		Name:      "eligible_event_completion_seconds",
		Help:      "Time from informer observation to completion of an eligible reconciliation work item.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"controller", "source", "outcome"})
	eligibleCompletions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kubernetes_secret_generator",
		Name:      "eligible_event_completions_total",
		Help:      "Completion outcomes for informer-observed eligible events; excluded marks invalid specifications outside the SLI denominator.",
	}, []string{"controller", "source", "outcome"})
	apiConnectivity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "kubernetes_secret_generator",
		Name:      "api_connectivity",
		Help:      "Whether the most recent controller API read completed without a connectivity error.",
	}, []string{"controller"})
	observedEvents = eventTracker{events: map[eventKey][]observedEvent{}}
)

func init() {
	controllermetrics.Registry.MustRegister(domainOutcomes, eligibleEvents, eligibleCompletions, completionLatency, apiConnectivity)
}

type eventKey struct {
	controller, namespace, name string
}

type observedEvent struct {
	source                 string
	uid                    types.UID
	subject                string
	subjectUID             types.UID
	resourceVersion        string
	subjectResourceVersion string
	at                     time.Time
	automatic              bool
	confirmed              bool
	suspicious             bool
	dataChanged            bool
	trackingChanged        bool
	apiUnavailable         bool
}

type eventTracker struct {
	mu     sync.Mutex
	events map[eventKey][]observedEvent
	adds   uint64
}

type expectedWrite struct {
	uid             types.UID
	resourceVersion string
	expires         time.Time
}

var expectedWrites = struct {
	sync.Mutex
	values map[string]expectedWrite
}{values: map[string]expectedWrite{}}

const (
	expectedWriteTTL  = time.Minute
	maxExpectedWrites = 1024
)

type observabilityContext struct {
	controller       string
	recorder         record.EventRecorder
	outcome          string
	events           []observedEvent
	uid              types.UID
	bound            bool
	dependent        string
	dependentUID     types.UID
	dependentBound   bool
	dependentMissing bool
}

type observabilityContextKey struct{}

// StartReconcile attaches redacted event recording and returns a completion
// callback for informer-observed work. Controller-runtime remains authoritative
// for generic reconcile count, error, queue, and duration metrics.
func StartReconcile(ctx context.Context, controller string, recorder record.EventRecorder, request types.NamespacedName) (context.Context, func(error)) {
	controller = metricController(controller)
	key := eventKey{controller: controller, namespace: request.Namespace, name: request.Name}
	observation := &observabilityContext{controller: controller, recorder: recorder, outcome: "success", events: observedEvents.take(key)}
	ctx = context.WithValue(ctx, observabilityContextKey{}, observation)
	var once sync.Once
	return ctx, func(reconcileErr error) {
		once.Do(func() {
			completed := time.Now()
			outcome := observation.outcome
			if reconcileErr != nil {
				outcome = "error"
			}
			for _, event := range observation.events {
				if outcome == "excluded" {
					continue
				}
				if !observation.bound || event.uid != observation.uid {
					if !observation.bound && reconcileErr != nil {
						event.apiUnavailable = event.apiUnavailable || !apiAvailable(reconcileErr)
						observedEvents.add(key, event)
					}
					continue
				}
				if reconcileErr != nil {
					event.apiUnavailable = event.apiUnavailable || !apiAvailable(reconcileErr)
					observedEvents.add(key, event)
					continue
				}
				if event.apiUnavailable {
					eligibleCompletions.WithLabelValues(controller, event.source, "excluded").Inc()
					completionLatency.WithLabelValues(controller, event.source, "excluded").Observe(completed.Sub(event.at).Seconds())
					ObserveDomainOutcome(controller, "sli", "excluded", "APIUnavailable")
					continue
				}
				if !event.automatic && !event.confirmed {
					if reconcileErr != nil {
						observedEvents.add(key, event)
					}
					continue
				}
				eligibleEvents.WithLabelValues(controller, event.source).Inc()
				eligibleCompletions.WithLabelValues(controller, event.source, outcome).Inc()
				completionLatency.WithLabelValues(controller, event.source, outcome).Observe(completed.Sub(event.at).Seconds())
			}
		})
	}
}

// BindReconcileObject prevents same-name delete/recreate events from being
// attributed to a different object incarnation.
func BindReconcileObject(ctx context.Context, object metav1.Object) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	obs.uid, obs.bound = object.GetUID(), true
}

func BindReconcileDependent(ctx context.Context, object metav1.Object) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	obs.dependent, obs.dependentUID, obs.dependentBound, obs.dependentMissing = object.GetName(), object.GetUID(), true, false
}

func BindReconcileDependentMissing(ctx context.Context, name string) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	obs.dependent, obs.dependentUID, obs.dependentBound, obs.dependentMissing = name, "", true, true
}

func ObserveEligibleEvent(controller, source string, object metav1.Object) {
	controller, source = metricController(controller), metricSource(source)
	key := eventKey{controller: controller, namespace: object.GetNamespace(), name: object.GetName()}
	observedEvents.add(key, observedEvent{source: source, uid: object.GetUID(), at: time.Now(), automatic: true})
}

// ObserveEligibleCandidate preserves the informer timestamp for a Secret
// event without adding it to the SLI denominator. Reconcile must confirm that
// the observed state requires work; controller self-writes are discarded.
func ObserveEligibleCandidate(controller, source string, object metav1.Object) {
	observeEligibleCandidate(controller, source, object, false, false)
}

func ObserveEligibleCandidateChanges(controller, source string, object metav1.Object, dataChanged, trackingChanged bool) {
	observeEligibleCandidate(controller, source, object, dataChanged, trackingChanged)
}

func observeEligibleCandidate(controller, source string, object metav1.Object, dataChanged, trackingChanged bool) {
	controller, source = metricController(controller), metricSource(source)
	key := eventKey{controller: controller, namespace: object.GetNamespace(), name: object.GetName()}
	observedEvents.add(key, observedEvent{
		source: source, uid: object.GetUID(), resourceVersion: object.GetResourceVersion(), at: time.Now(), dataChanged: dataChanged,
		trackingChanged: trackingChanged, suspicious: dataChanged && trackingChanged,
	})
}

func ObserveOwnedSecretCandidate(controller, source string, owner, secret metav1.Object, dataChanged, trackingChanged bool) {
	controller, source = metricController(controller), metricSource(source)
	key := eventKey{controller: controller, namespace: owner.GetNamespace(), name: owner.GetName()}
	observedEvents.add(key, observedEvent{
		source: source, uid: owner.GetUID(), subject: secret.GetName(), subjectUID: secret.GetUID(), at: time.Now(),
		subjectResourceVersion: secret.GetResourceVersion(),
		dataChanged:            dataChanged, trackingChanged: trackingChanged, suspicious: dataChanged && trackingChanged,
	})
}

func ConfirmEligibleEvent(ctx context.Context, source string) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	source = metricSource(source)
	for i := range obs.events {
		event := &obs.events[i]
		if candidateMatches(obs, event, source) {
			event.confirmed = true
		}
	}
}

func HasSuspiciousCandidate(ctx context.Context, source string) bool {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return false
	}
	source = metricSource(source)
	for i := range obs.events {
		if obs.events[i].suspicious && candidateMatches(obs, &obs.events[i], source) {
			return true
		}
	}
	return false
}

func candidateMatches(obs *observabilityContext, event *observedEvent, source string) bool {
	if !obs.bound || event.source != source || event.uid != obs.uid {
		return false
	}
	if event.subject == "" {
		return true
	}
	if !obs.dependentBound || event.subject != obs.dependent || obs.dependentMissing && source != "secret_delete" {
		return false
	}
	return obs.dependentMissing && event.subjectUID != "" || !obs.dependentMissing && event.subjectUID == obs.dependentUID
}

func RecordControllerWrite(ctx context.Context, object metav1.Object) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok || object.GetResourceVersion() == "" {
		return
	}
	key := writeKey(obs.controller, object)
	expectedWrites.Lock()
	if _, exists := expectedWrites.values[key]; !exists && len(expectedWrites.values) >= maxExpectedWrites {
		for oldest := range expectedWrites.values {
			delete(expectedWrites.values, oldest)
			break
		}
	}
	expectedWrites.values[key] = expectedWrite{uid: object.GetUID(), resourceVersion: object.GetResourceVersion(), expires: time.Now().Add(expectedWriteTTL)}
	expectedWrites.Unlock()
}

func ConsumeControllerWrite(controller string, object metav1.Object) bool {
	key := writeKey(metricController(controller), object)
	expectedWrites.Lock()
	expected, ok := expectedWrites.values[key]
	if !ok || time.Now().After(expected.expires) || expected.uid != object.GetUID() {
		delete(expectedWrites.values, key)
	} else if expected.resourceVersion == object.GetResourceVersion() {
		delete(expectedWrites.values, key)
	}
	expectedWrites.Unlock()
	return ok && time.Now().Before(expected.expires) && expected.uid == object.GetUID() && expected.resourceVersion == object.GetResourceVersion()
}

// ResolveControllerWrite closes the narrow race where the cache observes a
// successful patch before the client call records its returned resourceVersion.
func ResolveControllerWrite(ctx context.Context, controller, source string, object metav1.Object) bool {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return false
	}
	source = metricSource(source)
	key := writeKey(metricController(controller), object)
	expectedWrites.Lock()
	expected, exists := expectedWrites.values[key]
	expectedWrites.Unlock()
	if !exists || time.Now().After(expected.expires) || expected.uid != object.GetUID() {
		return false
	}
	matched := false
	for i := range obs.events {
		matched = matched || obs.events[i].suspicious && candidateMatches(obs, &obs.events[i], source) &&
			obs.events[i].subjectUID == expected.uid && obs.events[i].subjectResourceVersion == expected.resourceVersion
	}
	if !matched {
		return false
	}
	expectedWrites.Lock()
	delete(expectedWrites.values, key)
	expectedWrites.Unlock()
	for i := range obs.events {
		if candidateMatches(obs, &obs.events[i], source) {
			obs.events[i].dataChanged = false
			obs.events[i].trackingChanged = false
			obs.events[i].suspicious = false
		}
	}
	return true
}

// ResolveCandidateWrite clears candidate risk after a package-local exact
// UID/resourceVersion write intent closes the same watch-before-return race.
func ResolveCandidateWrite(ctx context.Context, source string, uid types.UID, resourceVersion string) bool {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return false
	}
	source = metricSource(source)
	resolved := false
	for i := range obs.events {
		if candidateMatches(obs, &obs.events[i], source) && obs.events[i].uid == uid && obs.events[i].resourceVersion == resourceVersion {
			obs.events[i].dataChanged = false
			obs.events[i].trackingChanged = false
			obs.events[i].suspicious = false
			resolved = true
		}
	}
	return resolved
}

func writeKey(controller string, object metav1.Object) string {
	return controller + "/" + object.GetNamespace() + "/" + object.GetName()
}

func ObserveAPIResult(controller string, err error) {
	value := float64(0)
	if apiAvailable(err) {
		value = 1
	}
	apiConnectivity.WithLabelValues(metricController(controller)).Set(value)
}

func apiAvailable(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) {
		return false
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return false
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return false
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Code >= 500 {
		return false
	}
	return true
}

func (t *eventTracker) add(key eventKey, event observedEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if event.at.IsZero() || now.Sub(event.at) > pendingEventTTL {
		recordPendingDrop(key, event, "CandidateExpired", now)
		return
	}
	t.adds++
	if t.adds%pendingSweepInterval == 0 {
		t.pruneAll(now)
	}
	if _, exists := t.events[key]; !exists && len(t.events) >= maxPendingEventKeys {
		t.pruneAll(now)
		if len(t.events) >= maxPendingEventKeys {
			t.evictOldest(now)
		}
	}
	events := t.prune(key, t.events[key], now)
	for i := range events {
		if events[i].source == event.source && events[i].uid == event.uid && events[i].subject == event.subject && events[i].subjectUID == event.subjectUID {
			if event.at.Before(events[i].at) {
				events[i].at = event.at
			}
			events[i].automatic = events[i].automatic || event.automatic
			events[i].confirmed = events[i].confirmed || event.confirmed
			events[i].dataChanged = events[i].dataChanged || event.dataChanged
			events[i].trackingChanged = events[i].trackingChanged || event.trackingChanged
			events[i].suspicious = events[i].suspicious || event.suspicious || events[i].dataChanged && events[i].trackingChanged
			events[i].apiUnavailable = events[i].apiUnavailable || event.apiUnavailable
			if event.resourceVersion != "" {
				events[i].resourceVersion = event.resourceVersion
			}
			if event.subjectResourceVersion != "" {
				events[i].subjectResourceVersion = event.subjectResourceVersion
			}
			t.events[key] = events
			return
		}
	}
	if len(events) == maxPendingEventsPerObject {
		recordPendingDrop(key, events[0], "CandidateEvicted", now)
		events = events[1:]
	}
	t.events[key] = append(events, event)
}

func (t *eventTracker) take(key eventKey) []observedEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	events := t.prune(key, t.events[key], time.Now())
	delete(t.events, key)
	return events
}

const (
	pendingEventTTL           = 5 * time.Minute
	maxPendingEventsPerObject = 16
	maxPendingEventKeys       = 1024
	pendingSweepInterval      = 64
)

func (t *eventTracker) prune(key eventKey, events []observedEvent, now time.Time) []observedEvent {
	kept := events[:0]
	for _, event := range events {
		if !event.at.IsZero() && now.Sub(event.at) <= pendingEventTTL {
			kept = append(kept, event)
		} else {
			recordPendingDrop(key, event, "CandidateExpired", now)
		}
	}
	return kept
}

func (t *eventTracker) pruneAll(now time.Time) {
	for key, events := range t.events {
		if events = t.prune(key, events, now); len(events) == 0 {
			delete(t.events, key)
		} else {
			t.events[key] = events
		}
	}
}

func (t *eventTracker) evictOldest(now time.Time) {
	var oldestKey eventKey
	var oldest time.Time
	for key, events := range t.events {
		for _, event := range events {
			if oldest.IsZero() || event.at.Before(oldest) {
				oldestKey, oldest = key, event.at
			}
		}
	}
	for _, event := range t.events[oldestKey] {
		recordPendingDrop(oldestKey, event, "CandidateEvicted", now)
	}
	delete(t.events, oldestKey)
}

func recordPendingDrop(key eventKey, event observedEvent, reason string, now time.Time) {
	controller, source := metricController(key.controller), metricSource(event.source)
	eligibleEvents.WithLabelValues(controller, source).Inc()
	eligibleCompletions.WithLabelValues(controller, source, "error").Inc()
	completionLatency.WithLabelValues(controller, source, "error").Observe(now.Sub(event.at).Seconds())
	ObserveDomainOutcome(controller, "sli", "error", reason)
}

func ObserveDomainOutcome(controller, operation, outcome, reason string) {
	domainOutcomes.WithLabelValues(metricController(controller), metricOperation(operation), metricOutcome(outcome), metricReason(reason)).Inc()
}

type EventObject interface {
	metav1.Object
	runtime.Object
}

func RecordRecreated(ctx context.Context, object EventObject) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	ObserveDomainOutcome(obs.controller, "recreate", "success", EventReasonSecretRecreated)
	if obs.recorder != nil {
		obs.recorder.Event(object, corev1.EventTypeNormal, EventReasonSecretRecreated, "Managed Secret was recreated with new generated credentials")
	}
}

func RecordRegenerated(ctx context.Context, object EventObject) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	ObserveDomainOutcome(obs.controller, "rotate", "success", EventReasonRegenerated)
	if obs.recorder != nil {
		obs.recorder.Event(object, corev1.EventTypeNormal, EventReasonRegenerated, "Managed generated credentials were rotated")
	}
}

func RecordConditionTransition(ctx context.Context, object EventObject, old *metav1.Condition, status metav1.ConditionStatus, reason string) {
	SetConditionOutcome(ctx, status, reason)
	if old != nil && old.Status == status && old.Reason == reason && old.ObservedGeneration == object.GetGeneration() {
		return
	}
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	if status == metav1.ConditionTrue {
		return
	}
	ObserveDomainOutcome(obs.controller, "condition", "terminal", reason)
	if obs.recorder == nil {
		return
	}
	switch reason {
	case "InvalidSpec":
		obs.recorder.Event(object, corev1.EventTypeWarning, reason, "Secret generator specification is invalid")
	case "SecretOwnershipConflict":
		obs.recorder.Event(object, corev1.EventTypeWarning, reason, "Same-name Secret has a conflicting controller owner")
	case "SecretTypeConflict":
		obs.recorder.Event(object, corev1.EventTypeWarning, reason, "Managed Secret has a conflicting creation-time type")
	case "ImmutableSecretConflict":
		obs.recorder.Event(object, corev1.EventTypeWarning, reason, "Immutable managed Secret requires reconciliation")
	case "TrackingStateConflict", "RegenerationStateConflict":
		obs.recorder.Event(object, corev1.EventTypeWarning, reason, "Managed Secret tracking state is inconsistent")
	}
}

func SetConditionOutcome(ctx context.Context, status metav1.ConditionStatus, reason string) {
	obs, ok := ctx.Value(observabilityContextKey{}).(*observabilityContext)
	if !ok {
		return
	}
	if status == metav1.ConditionTrue {
		obs.outcome = "success"
	} else if reason == "InvalidSpec" {
		obs.outcome = "excluded"
	} else {
		obs.outcome = "terminal"
	}
}

func metricController(value string) string {
	switch value {
	case ControllerStringSecret, ControllerBasicAuth, ControllerSSHKeyPair, ControllerAnnotation, ControllerManager:
		return value
	default:
		return "unknown"
	}
}

func metricOperation(value string) string {
	switch value {
	case "condition", "create", "recreate", "rotate", "sli":
		return value
	default:
		return "unknown"
	}
}

func metricOutcome(value string) string {
	switch value {
	case "success", "terminal", "error", "excluded":
		return value
	default:
		return "unknown"
	}
}

func metricReason(value string) string {
	switch value {
	case EventReasonSecretRecreated, EventReasonRegenerated,
		"Reconciled", "InvalidSpec", "LegacyBaselineInvalid", "SecretOwnershipConflict",
		"RegenerationStateConflict", "TrackingStateConflict", "SecretSizeConflict",
		"SecretTypeConflict", "ImmutableSecretConflict", "GenerationFailed", "ApplyFailed",
		"CandidateExpired", "CandidateEvicted", "APIUnavailable":
		return value
	default:
		return "unknown"
	}
}

func metricSource(value string) string {
	switch value {
	case "resource_create", "resource_update", "resource_delete", "secret_delete", "secret_drift":
		return value
	default:
		return "unknown"
	}
}
