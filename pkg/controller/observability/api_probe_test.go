package observability

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestAPIConnectivityProbeImmediateTickRecoveryAndCancel(t *testing.T) {
	apiConnectivity.Reset()
	ticks := make(chan time.Time)
	observed := make(chan struct{}, 3)
	var calls atomic.Int32
	probe := &APIConnectivityProbe{
		interval: time.Second,
		timeout:  100 * time.Millisecond,
		ticks:    ticks,
		probe: func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("probe context has no timeout")
			}
			if calls.Add(1) == 1 {
				return apierrors.NewServiceUnavailable("unavailable")
			}
			return nil
		},
		after: func(error) { observed <- struct{}{} },
	}
	if probe.NeedLeaderElection() {
		t.Fatal("probe must run on passive replicas")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- probe.Start(ctx) }()
	<-observed
	if got := metricValue(t, apiConnectivity.WithLabelValues(ControllerManager), "gauge"); got != 0 {
		t.Fatalf("initial unavailable gauge = %v, want 0", got)
	}
	ticks <- time.Now()
	<-observed
	if got := metricValue(t, apiConnectivity.WithLabelValues(ControllerManager), "gauge"); got != 1 {
		t.Fatalf("recovered gauge = %v, want 1", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe calls = %d, want immediate + one tick", got)
	}
}

func TestAPIConnectivityProbeTimeoutIsShorterThanInterval(t *testing.T) {
	if defaultAPIProbeTimeout >= defaultAPIProbeInterval {
		t.Fatalf("timeout %s must be shorter than interval %s", defaultAPIProbeTimeout, defaultAPIProbeInterval)
	}
}
