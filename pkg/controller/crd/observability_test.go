package crd

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	controllerobservability "github.com/mittwald/kubernetes-secret-generator/pkg/controller/observability"
)

func TestConditionEventIsTransitionOnlyAndRedacted(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "StringSecret"},
		ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: "default", Generation: 1},
		Spec:       v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value"}}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	recorder := record.NewFakeRecorder(2)
	ctx, complete := controllerobservability.StartReconcile(context.Background(), controllerobservability.ControllerStringSecret, recorder, types.NamespacedName{Namespace: "default", Name: "invalid"})
	defer complete(nil)
	cc := Client{Client: client}
	message := "KSG_TEST_SECRET_must-not-be-in-event"
	if err := cc.SetStatus(ctx, instance, nil, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, message, false, -1); err != nil {
		t.Fatal(err)
	}
	persisted := &v1alpha1.StringSecret{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Status.Conditions) != 1 {
		t.Fatal("unexpected condition count")
	}
	condition := persisted.Status.Conditions[0]
	if condition.Type != v1alpha1.ConditionTypeReady || condition.Reason != v1alpha1.ReasonInvalidSpec || condition.Status != metav1.ConditionFalse {
		t.Fatal("unexpected condition fields")
	}
	if err := cc.SetStatus(ctx, persisted, nil, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, message, false, -1); err != nil {
		t.Fatal(err)
	}
	refetched := &v1alpha1.StringSecret{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, refetched); err != nil {
		t.Fatal(err)
	}
	if len(refetched.Status.Conditions) != 1 {
		t.Fatal("unexpected stable condition count")
	}
	condition = refetched.Status.Conditions[0]
	if condition.Type != v1alpha1.ConditionTypeReady || condition.Reason != v1alpha1.ReasonInvalidSpec || condition.Status != metav1.ConditionFalse {
		t.Fatal("unexpected stable condition fields")
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, v1alpha1.ReasonInvalidSpec) || strings.Contains(event, "KSG_TEST_SECRET") {
			t.Fatal("event was not reason-only and redacted")
		}
	default:
		t.Fatal("condition transition emitted no Event")
	}
	select {
	case <-recorder.Events:
		t.Fatal("stable condition emitted duplicate Event")
	default:
	}
}

func TestImmutableConditionEventIsTransitionOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := &v1alpha1.StringSecret{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "StringSecret"},
		ObjectMeta: metav1.ObjectMeta{Name: "immutable", Namespace: "default", UID: "owner", Generation: 1},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance).Build()
	recorder := record.NewFakeRecorder(2)
	ctx, complete := controllerobservability.StartReconcile(context.Background(), controllerobservability.ControllerStringSecret, recorder, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name})
	defer complete(nil)
	controllerobservability.BindReconcileObject(ctx, instance)
	cc := Client{Client: client}
	if err := cc.SetStatus(ctx, instance, nil, metav1.ConditionFalse, v1alpha1.ReasonImmutableSecretConflict, "KSG_TEST_SECRET", true, -1); err != nil {
		t.Fatal(err)
	}
	persisted := &v1alpha1.StringSecret{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Status.Conditions) != 1 {
		t.Fatal("unexpected immutable condition count")
	}
	condition := persisted.Status.Conditions[0]
	if condition.Type != v1alpha1.ConditionTypeReady || condition.Reason != v1alpha1.ReasonImmutableSecretConflict || condition.Status != metav1.ConditionFalse {
		t.Fatal("unexpected immutable condition fields")
	}
	if err := cc.SetStatus(ctx, persisted, nil, metav1.ConditionFalse, v1alpha1.ReasonImmutableSecretConflict, "KSG_TEST_SECRET", true, -1); err != nil {
		t.Fatal(err)
	}
	refetched := &v1alpha1.StringSecret{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, refetched); err != nil {
		t.Fatal(err)
	}
	if len(refetched.Status.Conditions) != 1 {
		t.Fatal("unexpected stable immutable condition count")
	}
	condition = refetched.Status.Conditions[0]
	if condition.Type != v1alpha1.ConditionTypeReady || condition.Reason != v1alpha1.ReasonImmutableSecretConflict || condition.Status != metav1.ConditionFalse {
		t.Fatal("unexpected stable immutable condition fields")
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, v1alpha1.ReasonImmutableSecretConflict) || strings.Contains(event, "KSG_TEST_SECRET") {
			t.Fatal("immutable event was not redacted")
		}
	default:
		t.Fatal("immutable transition emitted no Event")
	}
	select {
	case <-recorder.Events:
		t.Fatal("immutable stable condition emitted duplicate Event")
	default:
	}
}
