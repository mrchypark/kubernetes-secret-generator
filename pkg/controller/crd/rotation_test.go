package crd

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestScheduleRotation(t *testing.T) {
	t.Parallel()

	anchor := time.Date(2026, 7, 14, 1, 2, 3, 4, time.UTC)
	tests := []struct {
		name        string
		interval    string
		annotations map[string]string
		now         time.Time
		wantRotate  bool
		wantRequeue time.Duration
		wantAnchor  string
		wantErr     bool
	}{
		{
			name: "default off",
			now:  anchor,
		},
		{
			name:        "first enable anchors without rotation",
			interval:    "1h",
			now:         anchor,
			wantRequeue: time.Hour,
			wantAnchor:  anchor.Format(time.RFC3339Nano),
		},
		{
			name:        "restart uses persisted anchor",
			interval:    "1h",
			annotations: map[string]string{RotationAnchorAnnotation: anchor.Format(time.RFC3339Nano)},
			now:         anchor.Add(20 * time.Minute),
			wantRequeue: 40 * time.Minute,
			wantAnchor:  anchor.Format(time.RFC3339Nano),
		},
		{
			name:        "due rotates once",
			interval:    "1h",
			annotations: map[string]string{RotationAnchorAnnotation: anchor.Format(time.RFC3339Nano)},
			now:         anchor.Add(time.Hour),
			wantRotate:  true,
			wantRequeue: time.Hour,
			wantAnchor:  anchor.Add(time.Hour).Format(time.RFC3339Nano),
		},
		{
			name:        "missed intervals coalesce",
			interval:    "1h",
			annotations: map[string]string{RotationAnchorAnnotation: anchor.Format(time.RFC3339Nano)},
			now:         anchor.Add(25 * time.Hour),
			wantRotate:  true,
			wantRequeue: time.Hour,
			wantAnchor:  anchor.Add(25 * time.Hour).Format(time.RFC3339Nano),
		},
		{
			name:        "shortened interval keeps anchor and becomes due",
			interval:    "30m",
			annotations: map[string]string{RotationAnchorAnnotation: anchor.Format(time.RFC3339Nano)},
			now:         anchor.Add(45 * time.Minute),
			wantRotate:  true,
			wantRequeue: 30 * time.Minute,
			wantAnchor:  anchor.Add(45 * time.Minute).Format(time.RFC3339Nano),
		},
		{
			name:        "extended interval keeps anchor",
			interval:    "2h",
			annotations: map[string]string{RotationAnchorAnnotation: anchor.Format(time.RFC3339Nano)},
			now:         anchor.Add(45 * time.Minute),
			wantRequeue: 75 * time.Minute,
			wantAnchor:  anchor.Format(time.RFC3339Nano),
		},
		{
			name:        "disable removes only anchor",
			annotations: map[string]string{RotationAnchorAnnotation: anchor.Format(time.RFC3339Nano), "keep": "yes"},
			now:         anchor,
		},
		{
			name:        "invalid duration",
			interval:    "tomorrow",
			annotations: map[string]string{"keep": "yes"},
			now:         anchor,
			wantErr:     true,
		},
		{
			name:     "below minimum",
			interval: "59s",
			now:      anchor,
			wantErr:  true,
		},
		{
			name:     "above maximum",
			interval: "8761h",
			now:      anchor,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := cloneAnnotations(tt.annotations)
			before := cloneAnnotations(annotations)
			secret := &corev1.Secret{}
			secret.Annotations = annotations

			result, rotate, err := ScheduleRotation(tt.interval, secret, tt.now)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ScheduleRotation() error = nil, want error")
				}
				if !equalAnnotations(secret.Annotations, before) {
					t.Fatalf("annotations mutated on error: got %v, want %v", secret.Annotations, before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if rotate != tt.wantRotate {
				t.Fatalf("rotate = %v, want %v", rotate, tt.wantRotate)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, tt.wantRequeue)
			}
			if got := secret.Annotations[RotationAnchorAnnotation]; got != tt.wantAnchor {
				t.Fatalf("anchor = %q, want %q", got, tt.wantAnchor)
			}
			if tt.name == "disable removes only anchor" && secret.Annotations["keep"] != "yes" {
				t.Fatal("unrelated annotation was changed")
			}
		})
	}
}

func TestSecretDataLossPredicate(t *testing.T) {
	t.Parallel()
	predicate := SecretDataLossPredicate()
	full := &corev1.Secret{Data: map[string][]byte{"generated": []byte("value"), "other": []byte("keep")}}

	if predicate.Create(event.TypedCreateEvent[*corev1.Secret]{Object: full}) {
		t.Fatal("create event should not enqueue")
	}
	if predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: full, ObjectNew: &corev1.Secret{Data: map[string][]byte{"generated": []byte("rotated"), "other": []byte("keep")}}}) {
		t.Fatal("nonempty value change should not enqueue")
	}
	if !predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: full, ObjectNew: &corev1.Secret{Data: map[string][]byte{"generated": nil, "other": []byte("keep")}}}) {
		t.Fatal("empty value should enqueue")
	}
	if !predicate.Update(event.TypedUpdateEvent[*corev1.Secret]{ObjectOld: full, ObjectNew: &corev1.Secret{Data: map[string][]byte{"other": []byte("keep")}}}) {
		t.Fatal("removed key should enqueue")
	}
	if !predicate.Delete(event.TypedDeleteEvent[*corev1.Secret]{Object: full}) {
		t.Fatal("delete event should enqueue")
	}
}

func cloneAnnotations(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func equalAnnotations(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
