package observability

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestLabelsAreBounded(t *testing.T) {
	domainOutcomes.Reset()
	ObserveDomainOutcome(ControllerStringSecret, "rotate", "success", EventReasonRegenerated)
	ObserveDomainOutcome("KSG_TEST_SECRET_namespace", "object-name", "secret-value", "checksum-value")
	if got := metricValue(t, domainOutcomes.WithLabelValues(ControllerStringSecret, "rotate", "success", EventReasonRegenerated), "counter"); got != 1 {
		t.Fatalf("known outcome = %v, want 1", got)
	}
	if got := metricValue(t, domainOutcomes.WithLabelValues("unknown", "unknown", "unknown", "unknown"), "counter"); got != 1 {
		t.Fatalf("unknown outcome = %v, want 1", got)
	}
	exposition := &dto.Metric{}
	if err := domainOutcomes.WithLabelValues("unknown", "unknown", "unknown", "unknown").Write(exposition); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"KSG_TEST_SECRET_namespace", "object-name", "secret-value", "checksum-value"} {
		if strings.Contains(exposition.String(), forbidden) {
			t.Fatalf("metric exposition contains %q", forbidden)
		}
	}
}

func TestSecretTypeConflictReasonIsPreserved(t *testing.T) {
	if got := metricReason("SecretTypeConflict"); got != "SecretTypeConflict" {
		t.Fatalf("metric reason = %q, want SecretTypeConflict", got)
	}
}

func TestInformerObservationCompletesLatencyAndDenominator(t *testing.T) {
	resetEventMetrics()
	object := &metav1.ObjectMeta{Name: "observed", Namespace: "default", UID: "uid-observed"}
	ObserveEligibleEvent(ControllerStringSecret, "resource_create", object)
	ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	complete(nil)
	if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerStringSecret, "resource_create"), "counter"); got != 1 {
		t.Fatalf("eligible event count = %v, want 1", got)
	}
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "resource_create", "success"), "counter"); got != 1 {
		t.Fatalf("completion count = %v, want 1", got)
	}
	if got := metricValue(t, completionLatency.WithLabelValues(ControllerStringSecret, "resource_create", "success").(interface{ Write(*dto.Metric) error }), "histogram"); got != 1 {
		t.Fatalf("latency sample count = %v, want 1", got)
	}
}

func TestSecretDriftCandidateCountsExternalWorkNotRepairSelfEvent(t *testing.T) {
	resetEventMetrics()
	object := &metav1.ObjectMeta{Name: "drift", Namespace: "default", UID: "uid-drift"}
	ObserveEligibleCandidate(ControllerStringSecret, "secret_drift", object)
	ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	ConfirmEligibleEvent(ctx, "secret_drift")
	complete(nil)

	ObserveEligibleCandidate(ControllerStringSecret, "secret_drift", object)
	ctx, complete = StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	complete(nil)
	if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerStringSecret, "secret_drift"), "counter"); got != 1 {
		t.Fatalf("eligible drift count = %v, want 1", got)
	}
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "secret_drift", "success"), "counter"); got != 1 {
		t.Fatalf("drift completion count = %v, want 1", got)
	}
}

func TestResourceUpdateDoesNotBecomeSecretDrift(t *testing.T) {
	resetEventMetrics()
	object := &metav1.ObjectMeta{Name: "force", Namespace: "default", UID: "uid-force"}
	ObserveEligibleEvent(ControllerStringSecret, "resource_update", object)
	ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	complete(nil)
	if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerStringSecret, "resource_update"), "counter"); got != 1 {
		t.Fatalf("resource update count = %v, want 1", got)
	}
	if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerStringSecret, "secret_drift"), "counter"); got != 0 {
		t.Fatalf("secret drift count = %v, want 0", got)
	}
}

func TestInvalidSpecIsExcludedBeforeEligibleDenominator(t *testing.T) {
	resetEventMetrics()
	object := &metav1.ObjectMeta{Name: "invalid", Namespace: "default", UID: "uid-invalid"}
	ObserveEligibleEvent(ControllerSSHKeyPair, "resource_create", object)
	ctx, complete := StartReconcile(context.Background(), ControllerSSHKeyPair, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	SetConditionOutcome(ctx, metav1.ConditionFalse, "InvalidSpec")
	complete(nil)
	if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerSSHKeyPair, "resource_create"), "counter"); got != 0 {
		t.Fatalf("invalid eligible count = %v, want 0", got)
	}
}

func TestCompletionOutcomeSampledExactlyOnce(t *testing.T) {
	for _, tt := range []struct {
		name    string
		outcome string
		err     error
	}{
		{name: "success", outcome: "success"},
		{name: "terminal", outcome: "terminal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetEventMetrics()
			object := &metav1.ObjectMeta{Name: tt.name, Namespace: "default", UID: types.UID("uid-" + tt.name)}
			ObserveEligibleEvent(ControllerBasicAuth, "resource_update", object)
			ctx, complete := StartReconcile(context.Background(), ControllerBasicAuth, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
			BindReconcileObject(ctx, object)
			if tt.outcome == "terminal" {
				SetConditionOutcome(ctx, metav1.ConditionFalse, "ImmutableSecretConflict")
			}
			complete(tt.err)
			complete(tt.err)
			if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerBasicAuth, "resource_update", tt.outcome), "counter"); got != 1 {
				t.Fatalf("completion count = %v, want 1", got)
			}
			if got := metricValue(t, completionLatency.WithLabelValues(ControllerBasicAuth, "resource_update", tt.outcome).(interface{ Write(*dto.Metric) error }), "histogram"); got != 1 {
				t.Fatalf("latency sample count = %v, want 1", got)
			}
		})
	}
}

func TestEventTrackerCoalescesUIDBoundsAndInFlightArrival(t *testing.T) {
	resetEventMetrics()
	key := eventKey{controller: ControllerStringSecret, namespace: "default", name: "coalesced"}
	first := time.Now().Add(-time.Second)
	for range 5 {
		observedEvents.add(key, observedEvent{source: "secret_drift", uid: "same", at: first})
	}
	if got := len(observedEvents.events[key]); got != 1 {
		t.Fatalf("coalesced event count = %d, want 1", got)
	}
	for i := range maxPendingEventsPerObject + 3 {
		observedEvents.add(key, observedEvent{source: "secret_drift", uid: types.UID(fmt.Sprintf("uid-%d", i)), at: time.Now()})
	}
	if got := len(observedEvents.events[key]); got != maxPendingEventsPerObject {
		t.Fatalf("bounded event count = %d, want %d", got, maxPendingEventsPerObject)
	}

	resetEventMetrics()
	object := &metav1.ObjectMeta{Name: "in-flight", Namespace: "default", UID: "uid-in-flight"}
	ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	ObserveEligibleCandidate(ControllerStringSecret, "secret_drift", object)
	ConfirmEligibleEvent(ctx, "secret_drift")
	complete(nil)
	ctx, complete = StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	ConfirmEligibleEvent(ctx, "secret_drift")
	complete(nil)
	if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerStringSecret, "secret_drift"), "counter"); got != 1 {
		t.Fatalf("in-flight eligible count = %v, want 1", got)
	}
}

func TestSameNameDifferentUIDIsNotAttributed(t *testing.T) {
	resetEventMetrics()
	old := &metav1.ObjectMeta{Name: "recreated", Namespace: "default", UID: "old-uid"}
	current := old.DeepCopy()
	current.UID = "new-uid"
	ObserveEligibleCandidate(ControllerStringSecret, "secret_drift", old)
	ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: old.Namespace, Name: old.Name})
	BindReconcileObject(ctx, current)
	ConfirmEligibleEvent(ctx, "secret_drift")
	complete(nil)
	if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerStringSecret, "secret_drift"), "counter"); got != 0 {
		t.Fatalf("different UID eligible count = %v, want 0", got)
	}
}

func TestOwnedSecretRequiresOwnerAndSubjectIdentity(t *testing.T) {
	for _, tt := range []struct {
		name          string
		currentOwner  types.UID
		currentSecret types.UID
	}{
		{name: "owner changed", currentOwner: "new-owner", currentSecret: "secret-uid"},
		{name: "secret recreated", currentOwner: "owner-uid", currentSecret: "new-secret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetEventMetrics()
			owner := &metav1.ObjectMeta{Name: "managed", Namespace: "default", UID: "owner-uid"}
			secret := &metav1.ObjectMeta{Name: "managed", Namespace: "default", UID: "secret-uid"}
			ObserveOwnedSecretCandidate(ControllerStringSecret, "secret_drift", owner, secret, false, false)
			ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: owner.Namespace, Name: owner.Name})
			currentOwner := owner.DeepCopy()
			currentOwner.UID = tt.currentOwner
			currentSecret := secret.DeepCopy()
			currentSecret.UID = tt.currentSecret
			BindReconcileObject(ctx, currentOwner)
			BindReconcileDependent(ctx, currentSecret)
			ConfirmEligibleEvent(ctx, "secret_drift")
			complete(nil)
			if got := metricValue(t, eligibleEvents.WithLabelValues(ControllerStringSecret, "secret_drift"), "counter"); got != 0 {
				t.Fatalf("mismatched identity eligible count = %v, want 0", got)
			}
		})
	}
}

func TestReturnedWriteRVResolvesCandidateAfterUnrelatedObjectUpdate(t *testing.T) {
	resetEventMetrics()
	expectedWrites.Lock()
	expectedWrites.values = map[string]expectedWrite{}
	expectedWrites.Unlock()
	owner := &metav1.ObjectMeta{Name: "managed", Namespace: "default", UID: "owner-uid"}
	selfWrite := &metav1.ObjectMeta{Name: "managed", Namespace: "default", UID: "secret-uid", ResourceVersion: "10"}
	ObserveOwnedSecretCandidate(ControllerStringSecret, "secret_drift", owner, selfWrite, true, true)
	writeCtx, _ := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: "other", Name: "other"})
	RecordControllerWrite(writeCtx, selfWrite)

	ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: owner.Namespace, Name: owner.Name})
	BindReconcileObject(ctx, owner)
	current := selfWrite.DeepCopy()
	current.ResourceVersion = "11"
	BindReconcileDependent(ctx, current)
	if !ResolveControllerWrite(ctx, ControllerStringSecret, "secret_drift", current) {
		t.Fatal("exact candidate resourceVersion was not resolved")
	}
	if HasSuspiciousCandidate(ctx, "secret_drift") {
		t.Fatal("resolved controller candidate remained suspicious")
	}
	complete(nil)
}

func TestReconcileErrorsRetainEpisodeUntilRecovery(t *testing.T) {
	resetEventMetrics()
	object := &metav1.ObjectMeta{Name: "reachable-error", Namespace: "default", UID: "uid-error"}
	ObserveEligibleEvent(ControllerStringSecret, "resource_update", object)
	_, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	complete(apierrors.NewForbidden(schema.GroupResource{Resource: "stringsecrets"}, object.Name, errors.New("denied")))
	ctx, complete := StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	complete(nil)
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "resource_update", "success"), "counter"); got != 1 {
		t.Fatalf("reachable error recovery completion = %v, want 1", got)
	}
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "resource_update", "error"), "counter"); got != 0 {
		t.Fatalf("reachable attempt error completion = %v, want 0", got)
	}

	resetEventMetrics()
	object.Name = "unavailable-retry"
	ObserveEligibleEvent(ControllerStringSecret, "resource_update", object)
	_, complete = StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	complete(apierrors.NewServiceUnavailable("unavailable"))
	ctx, complete = StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	complete(nil)
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "resource_update", "success"), "counter"); got != 0 {
		t.Fatalf("outage-overlapped success completion = %v, want 0", got)
	}
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "resource_update", "excluded"), "counter"); got != 1 {
		t.Fatalf("outage exclusion completion = %v, want 1", got)
	}
	if got := metricValue(t, completionLatency.WithLabelValues(ControllerStringSecret, "resource_update", "success").(interface{ Write(*dto.Metric) error }), "histogram"); got != 0 {
		t.Fatalf("outage-overlapped success latency count = %v, want 0", got)
	}
	if got := metricValue(t, domainOutcomes.WithLabelValues(ControllerStringSecret, "sli", "excluded", "APIUnavailable"), "counter"); got != 1 {
		t.Fatalf("API unavailable exclusion outcome = %v, want 1", got)
	}

	resetEventMetrics()
	object.Name = "transient-retry"
	ObserveEligibleEvent(ControllerStringSecret, "resource_update", object)
	ctx, complete = StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	complete(errors.New("transient apply error"))
	ctx, complete = StartReconcile(context.Background(), ControllerStringSecret, nil, types.NamespacedName{Namespace: object.Namespace, Name: object.Name})
	BindReconcileObject(ctx, object)
	complete(nil)
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "resource_update", "success"), "counter"); got != 1 {
		t.Fatalf("transient recovery completion = %v, want 1", got)
	}
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "resource_update", "error"), "counter"); got != 0 {
		t.Fatalf("transient attempt error completion = %v, want 0", got)
	}
}

func TestExpiredCandidateIsObservableAndRemoved(t *testing.T) {
	resetEventMetrics()
	key := eventKey{controller: ControllerStringSecret, namespace: "default", name: "expired"}
	observedEvents.add(key, observedEvent{source: "secret_drift", uid: "uid", at: time.Now().Add(-pendingEventTTL - time.Second)})
	if _, exists := observedEvents.events[key]; exists {
		t.Fatal("expired candidate was retained")
	}
	if got := metricValue(t, eligibleCompletions.WithLabelValues(ControllerStringSecret, "secret_drift", "error"), "counter"); got != 1 {
		t.Fatalf("expired completion = %v, want 1", got)
	}
	if got := metricValue(t, domainOutcomes.WithLabelValues(ControllerStringSecret, "sli", "error", "CandidateExpired"), "counter"); got != 1 {
		t.Fatalf("expired outcome = %v, want 1", got)
	}
}

func TestAPIConnectivitySignal(t *testing.T) {
	apiConnectivity.Reset()
	tests := []struct {
		name string
		err  error
		want float64
	}{
		{name: "healthy", want: 1},
		{name: "not found", err: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "name"), want: 1},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "name", errors.New("denied")), want: 1},
		{name: "conflict", err: apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "name", errors.New("conflict")), want: 1},
		{name: "too many requests", err: apierrors.NewTooManyRequests("busy", 1), want: 1},
		{name: "service unavailable", err: apierrors.NewServiceUnavailable("unavailable")},
		{name: "timeout", err: apierrors.NewTimeoutError("timeout", 1)},
		{name: "network", err: &url.Error{Op: "Get", URL: "https://api", Err: errors.New("connection refused")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ObserveAPIResult(ControllerBasicAuth, tt.err)
			if got := metricValue(t, apiConnectivity.WithLabelValues(ControllerBasicAuth), "gauge"); got != tt.want {
				t.Fatalf("connectivity gauge = %v, want %v", got, tt.want)
			}
		})
	}
}

func resetEventMetrics() {
	eligibleEvents.Reset()
	eligibleCompletions.Reset()
	completionLatency.Reset()
	observedEvents = eventTracker{events: map[eventKey][]observedEvent{}}
}

func metricValue(t *testing.T, metric interface{ Write(*dto.Metric) error }, kind string) float64 {
	t.Helper()
	value := &dto.Metric{}
	if err := metric.Write(value); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "counter":
		return value.GetCounter().GetValue()
	case "gauge":
		return value.GetGauge().GetValue()
	default:
		return float64(value.GetHistogram().GetSampleCount())
	}
}
