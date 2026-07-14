package crd

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const RotationAnchorAnnotation = "secretgenerator.mittwald.de/rotation-anchor"

const (
	minimumRotationInterval = time.Minute
	maximumRotationInterval = 8760 * time.Hour
)

// ScheduleRotation updates only the persisted rotation anchor on target. It
// returns whether generated credentials are due for one regeneration.
func ScheduleRotation(interval string, target *corev1.Secret, now time.Time) (reconcile.Result, bool, error) {
	if interval == "" {
		if target.Annotations != nil {
			delete(target.Annotations, RotationAnchorAnnotation)
		}
		return reconcile.Result{}, false, nil
	}

	duration, err := time.ParseDuration(interval)
	if err != nil {
		return reconcile.Result{}, false, fmt.Errorf("invalid rotationInterval %q: %w", interval, err)
	}
	if duration < minimumRotationInterval || duration > maximumRotationInterval {
		return reconcile.Result{}, false, fmt.Errorf("rotationInterval must be between %s and %s", minimumRotationInterval, maximumRotationInterval)
	}

	anchorValue := target.Annotations[RotationAnchorAnnotation]
	if anchorValue == "" {
		setRotationAnchor(target, now)
		return reconcile.Result{RequeueAfter: duration}, false, nil
	}

	anchor, err := time.Parse(time.RFC3339Nano, anchorValue)
	if err != nil {
		return reconcile.Result{}, false, fmt.Errorf("invalid %s annotation %q: %w", RotationAnchorAnnotation, anchorValue, err)
	}
	next := anchor.Add(duration)
	if now.Before(next) {
		return reconcile.Result{RequeueAfter: next.Sub(now)}, false, nil
	}

	// Coalesce any missed intervals into one regeneration and start the next
	// interval from the successful reconciliation attempt.
	setRotationAnchor(target, now)
	return reconcile.Result{RequeueAfter: duration}, true, nil
}

func setRotationAnchor(target *corev1.Secret, now time.Time) {
	if target.Annotations == nil {
		target.Annotations = make(map[string]string)
	}
	target.Annotations[RotationAnchorAnnotation] = now.UTC().Format(time.RFC3339Nano)
}
