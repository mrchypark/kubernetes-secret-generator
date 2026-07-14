package stringsecret

import (
	"bytes"
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
)

func TestRotationAndAdditiveSecretRepair(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 2, 3, 4, time.UTC)
	tests := []struct {
		name          string
		interval      string
		force         bool
		fields        []v1alpha1.Field
		secret        *corev1.Secret
		wantErr       bool
		wantChanged   bool
		wantLiteral   string
		wantUnmanaged string
		wantAnchor    string
		wantRequeue   time.Duration
	}{
		{
			name:          "default off preserves nonempty tamper",
			fields:        generatedFields(),
			secret:        ownedStringSecret(map[string][]byte{"generated": []byte("tampered"), "literal": []byte("literal"), "unmanaged": []byte("keep")}),
			wantLiteral:   "literal",
			wantUnmanaged: "keep",
		},
		{
			name:          "first enable anchors without rotation",
			interval:      "1h",
			fields:        generatedFields(),
			secret:        ownedStringSecret(map[string][]byte{"generated": []byte("old"), "literal": []byte("literal"), "unmanaged": []byte("keep")}),
			wantLiteral:   "literal",
			wantUnmanaged: "keep",
			wantAnchor:    now.Format(time.RFC3339Nano),
			wantRequeue:   time.Hour,
		},
		{
			name:     "due rotation changes only generated value",
			interval: "1h",
			fields:   generatedFields(),
			secret: withAnchor(
				ownedStringSecret(map[string][]byte{"generated": []byte("old"), "literal": []byte("literal"), "unmanaged": []byte("keep")}),
				now.Add(-25*time.Hour),
			),
			wantChanged:   true,
			wantLiteral:   "literal",
			wantUnmanaged: "keep",
			wantAnchor:    now.Format(time.RFC3339Nano),
			wantRequeue:   time.Hour,
		},
		{
			name:          "missing generated value is repaired",
			fields:        generatedFields(),
			secret:        ownedStringSecret(map[string][]byte{"literal": []byte("literal"), "unmanaged": []byte("keep")}),
			wantChanged:   true,
			wantLiteral:   "literal",
			wantUnmanaged: "keep",
		},
		{
			name:          "missing managed literal is repaired",
			fields:        generatedFields(),
			secret:        ownedStringSecret(map[string][]byte{"generated": []byte("old"), "unmanaged": []byte("keep")}),
			wantLiteral:   "literal",
			wantUnmanaged: "keep",
		},
		{
			name:          "empty managed literal is repaired",
			fields:        generatedFields(),
			secret:        ownedStringSecret(map[string][]byte{"generated": []byte("old"), "literal": nil, "unmanaged": []byte("keep")}),
			wantLiteral:   "literal",
			wantUnmanaged: "keep",
		},
		{
			name:          "nonempty managed literal is preserved",
			fields:        generatedFields(),
			secret:        ownedStringSecret(map[string][]byte{"generated": []byte("old"), "literal": []byte("tampered"), "unmanaged": []byte("keep")}),
			wantLiteral:   "tampered",
			wantUnmanaged: "keep",
		},
		{
			name:          "force behavior remains regenerative",
			force:         true,
			fields:        generatedFields(),
			secret:        ownedStringSecret(map[string][]byte{"generated": []byte("old"), "literal": []byte("old-literal"), "unmanaged": []byte("keep")}),
			wantChanged:   true,
			wantLiteral:   "literal",
			wantUnmanaged: "keep",
		},
		{
			name:        "invalid duration does not mutate",
			interval:    "tomorrow",
			fields:      generatedFields(),
			secret:      ownedStringSecret(map[string][]byte{"generated": []byte("old"), "literal": []byte("literal")}),
			wantErr:     true,
			wantLiteral: "literal",
		},
		{
			name:        "rotation without generated fields is rejected",
			interval:    "1h",
			secret:      ownedStringSecret(map[string][]byte{"literal": []byte("literal")}),
			wantErr:     true,
			wantLiteral: "literal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := stringSecretCR(tt.interval, tt.force, tt.fields)
			before := tt.secret.DeepCopy()
			c, scheme := stringSecretClient(t, cr, tt.secret)
			r := &ReconcileStringSecret{client: c, scheme: scheme, now: func() time.Time { return now }}

			result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
			if tt.wantErr {
				if err == nil {
					t.Fatal("Reconcile() error = nil, want error")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, tt.wantRequeue)
			}

			got := &corev1.Secret{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(tt.secret), got); err != nil {
				t.Fatal(err)
			}
			changed := !bytes.Equal(before.Data["generated"], got.Data["generated"])
			if changed != tt.wantChanged {
				t.Fatalf("generated changed = %v, want %v", changed, tt.wantChanged)
			}
			if string(got.Data["literal"]) != tt.wantLiteral || string(got.Data["unmanaged"]) != tt.wantUnmanaged {
				t.Fatalf("preserved data = %q/%q, want %q/%q", got.Data["literal"], got.Data["unmanaged"], tt.wantLiteral, tt.wantUnmanaged)
			}
			if got.Annotations[crd.RotationAnchorAnnotation] != tt.wantAnchor {
				t.Fatalf("anchor = %q, want %q", got.Annotations[crd.RotationAnchorAnnotation], tt.wantAnchor)
			}
			if got.Type != before.Type || !equalStringMap(got.Labels, before.Labels) || len(got.OwnerReferences) != len(before.OwnerReferences) {
				t.Fatal("type, labels, or owner references changed")
			}

			if tt.name == "due rotation changes only generated value" {
				again := got.DeepCopy()
				if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(tt.secret), got); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(again.Data["generated"], got.Data["generated"]) {
					t.Fatal("due interval rotated more than once")
				}
			}
		})
	}
}

func TestDeletedOwnedSecretIsRecreated(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 2, 3, 4, time.UTC)
	cr := stringSecretCR("1h", false, generatedFields())
	c, scheme := stringSecretClient(t, cr)
	r := &ReconcileStringSecret{client: c, scheme: scheme, now: func() time.Time { return now }}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != time.Hour {
		t.Fatalf("RequeueAfter = %s, want 1h", result.RequeueAfter)
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data["generated"]) == 0 || got.Annotations[crd.RotationAnchorAnnotation] != now.Format(time.RFC3339Nano) {
		t.Fatal("recreated Secret is missing generated data or rotation anchor")
	}
}

func stringSecretCR(interval string, force bool, fields []v1alpha1.Field) *v1alpha1.StringSecret {
	return &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "owner-uid"},
		Spec: v1alpha1.StringSecretSpec{
			Type:             string(corev1.SecretTypeOpaque),
			Data:             map[string]string{"literal": "literal"},
			ForceRegenerate:  force,
			RotationInterval: interval,
			Fields:           fields,
		},
	}
}

func generatedFields() []v1alpha1.Field {
	return []v1alpha1.Field{{FieldName: "generated", Encoding: "base64", Length: "24"}}
}

func ownedStringSecret(data map[string][]byte) *corev1.Secret {
	controller := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example",
			Namespace: "default",
			Labels:    map[string]string{"preserve": "yes"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind, Name: "example", UID: "owner-uid", Controller: &controller,
			}},
		},
		Type: corev1.SecretTypeTLS,
		Data: data,
	}
}

func withAnchor(secret *corev1.Secret, anchor time.Time) *corev1.Secret {
	secret.Annotations = map[string]string{crd.RotationAnchorAnnotation: anchor.Format(time.RFC3339Nano), "preserve": "yes"}
	return secret
}

func stringSecretClient(t *testing.T, objects ...client.Object) (client.Client, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(clientgoscheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.StringSecret{}).WithObjects(objects...).Build()
	return c, scheme
}

func equalStringMap(a, b map[string]string) bool {
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
