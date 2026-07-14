package basicauth

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
)

func TestRepairPartialBasicAuthData(t *testing.T) {
	password := []byte("existing-password")
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	auth := append([]byte("admin:"), hash...)

	tests := []struct {
		name    string
		field   string
		missing bool
		wantErr bool
	}{
		{"auth missing", secret.FieldBasicAuthIngress, true, false},
		{"auth empty", secret.FieldBasicAuthIngress, false, false},
		{"username missing", secret.FieldBasicAuthUsername, true, false},
		{"username empty", secret.FieldBasicAuthUsername, false, false},
		{"password missing", secret.FieldBasicAuthPassword, true, true},
		{"password empty", secret.FieldBasicAuthPassword, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := map[string][]byte{
				secret.FieldBasicAuthIngress:  bytes.Clone(auth),
				secret.FieldBasicAuthUsername: []byte("admin"),
				secret.FieldBasicAuthPassword: bytes.Clone(password),
			}
			if tt.missing {
				delete(data, tt.field)
			} else {
				data[tt.field] = nil
			}
			before := cloneByteMap(data)

			err := secret.RepairBasicAuthData(log, &secret.BasicAuthConstraints{Username: "admin", Length: "16", Encoding: "base64"}, data)
			if tt.wantErr {
				if err == nil {
					t.Fatal("RepairBasicAuthData() error = nil, want error")
				}
				if !reflect.DeepEqual(data, before) {
					t.Fatalf("data mutated on error: got %v, want %v", data, before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data[secret.FieldBasicAuthPassword], password) || string(data[secret.FieldBasicAuthUsername]) != "admin" {
				t.Fatal("remaining nonempty username or password changed")
			}
			parsedUser, parsedHash, ok := bytes.Cut(data[secret.FieldBasicAuthIngress], []byte(":"))
			if !ok || string(parsedUser) != "admin" || bcrypt.CompareHashAndPassword(parsedHash, password) != nil {
				t.Fatal("repaired auth is inconsistent")
			}
		})
	}

	for _, state := range []string{"missing", "empty"} {
		t.Run("auth and username "+state, func(t *testing.T) {
			data := map[string][]byte{secret.FieldBasicAuthPassword: bytes.Clone(password)}
			if state == "empty" {
				data[secret.FieldBasicAuthIngress] = nil
				data[secret.FieldBasicAuthUsername] = nil
			}
			if err := secret.RepairBasicAuthData(log, &secret.BasicAuthConstraints{Username: "admin"}, data); err != nil {
				t.Fatal(err)
			}
			parsedUser, parsedHash, ok := bytes.Cut(data[secret.FieldBasicAuthIngress], []byte(":"))
			if !ok || string(parsedUser) != "admin" || string(data[secret.FieldBasicAuthUsername]) != "admin" || !bytes.Equal(data[secret.FieldBasicAuthPassword], password) {
				t.Fatal("auth and username were not safely restored")
			}
			if err := bcrypt.CompareHashAndPassword(parsedHash, password); err != nil {
				t.Fatal("restored auth does not match preserved password")
			}
		})
	}

	t.Run("inconsistent auth and password does not mutate", func(t *testing.T) {
		data := map[string][]byte{secret.FieldBasicAuthIngress: bytes.Clone(auth), secret.FieldBasicAuthPassword: []byte("different")}
		before := cloneByteMap(data)
		if err := secret.RepairBasicAuthData(log, &secret.BasicAuthConstraints{}, data); err == nil {
			t.Fatal("RepairBasicAuthData() error = nil, want error")
		}
		if !reflect.DeepEqual(data, before) {
			t.Fatal("inconsistent state was mutated")
		}
	})
}

func cloneByteMap(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for key, value := range in {
		out[key] = bytes.Clone(value)
	}
	return out
}

func TestDueRotationPreservesLiteralAndUnmanagedData(t *testing.T) {
	now := time.Date(2026, 7, 14, 1, 2, 3, 4, time.UTC)
	controller := true
	cr := &v1alpha1.BasicAuth{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "owner-uid"},
		Spec: v1alpha1.BasicAuthSpec{
			Username: "admin", Length: "16", Encoding: "base64", Data: map[string]string{"literal": "literal"}, RotationInterval: "1h",
		},
	}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "example", Namespace: "default", Labels: map[string]string{"preserve": "yes"},
			Annotations:     map[string]string{crd.RotationAnchorAnnotation: now.Add(-2 * time.Hour).Format(time.RFC3339Nano)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: Kind, Name: "example", UID: "owner-uid", Controller: &controller}},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			secret.FieldBasicAuthIngress: []byte("old-auth"), secret.FieldBasicAuthUsername: []byte("admin"), secret.FieldBasicAuthPassword: []byte("old-password"),
			"literal": []byte("literal"), "unmanaged": []byte("keep"),
		},
	}

	c, scheme := basicAuthClient(t, cr, existing)
	r := &ReconcileBasicAuth{client: c, scheme: scheme, now: func() time.Time { return now }}
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != time.Hour {
		t.Fatalf("RequeueAfter = %s, want 1h", result.RequeueAfter)
	}

	got := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), got); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got.Data[secret.FieldBasicAuthPassword], existing.Data[secret.FieldBasicAuthPassword]) {
		t.Fatal("password was not rotated")
	}
	if string(got.Data["literal"]) != "literal" || string(got.Data["unmanaged"]) != "keep" {
		t.Fatal("literal or unmanaged data changed")
	}
	if got.Type != existing.Type || got.Labels["preserve"] != "yes" || got.OwnerReferences[0].UID != "owner-uid" {
		t.Fatal("type, labels, or owner changed")
	}
}

func basicAuthClient(t *testing.T, objects ...client.Object) (client.Client, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.BasicAuth{}).WithObjects(objects...).Build(), scheme
}
