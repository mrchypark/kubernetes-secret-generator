package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAPIObjectContracts(t *testing.T) {
	objects := []struct {
		name     string
		object   APIObject
		wantType string
	}{
		{name: "basic auth", object: &BasicAuth{}, wantType: string(corev1.SecretTypeOpaque)},
		{name: "ssh key pair", object: &SSHKeyPair{Spec: SSHKeyPairSpec{Type: string(corev1.SecretTypeSSHAuth)}}, wantType: string(corev1.SecretTypeSSHAuth)},
		{name: "string secret", object: &StringSecret{Spec: StringSecretSpec{Type: string(corev1.SecretTypeTLS)}}, wantType: string(corev1.SecretTypeTLS)},
	}
	for _, tt := range objects {
		t.Run(tt.name, func(t *testing.T) {
			ref := &corev1.ObjectReference{Name: "generated", Namespace: "default"}
			status := tt.object.GetStatus()
			status.SetSecret(ref)
			status.GetCommonStatus().ObservedGeneration = 9
			require.Same(t, ref, status.GetSecret())
			require.EqualValues(t, 9, tt.object.GetStatus().GetCommonStatus().ObservedGeneration)
			require.Equal(t, tt.wantType, tt.object.GetType())
		})
	}
}

func TestSSHKeyPairFieldDefaults(t *testing.T) {
	tests := []struct {
		name        string
		privateKey  string
		publicKey   string
		wantPrivate string
		wantPublic  string
	}{
		{name: "defaults", wantPrivate: "ssh-privatekey", wantPublic: "ssh-publickey"},
		{name: "overrides", privateKey: "identity", publicKey: "identity.pub", wantPrivate: "identity", wantPublic: "identity.pub"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := &SSHKeyPair{Spec: SSHKeyPairSpec{PrivateKeyField: tt.privateKey, PublicKeyField: tt.publicKey}}
			require.Equal(t, tt.wantPrivate, object.GetPrivateKeyField())
			require.Equal(t, tt.wantPublic, object.GetPublicKeyField())
		})
	}
}

func TestListMetadataAccessors(t *testing.T) {
	typeMeta := metav1.TypeMeta{APIVersion: SchemeGroupVersion.String(), Kind: "ExampleList"}
	listMeta := metav1.ListMeta{ResourceVersion: "7", Continue: "next"}

	basic := &BasicAuthList{}
	basic.SetTypeMeta(typeMeta)
	basic.SetListMeta(listMeta)
	require.Equal(t, typeMeta, basic.GetTypeMeta())
	require.Equal(t, listMeta, basic.GetListMeta())

	ssh := &SSHKeyPairList{}
	ssh.SetTypeMeta(typeMeta)
	ssh.SetListMeta(listMeta)
	require.Equal(t, typeMeta, ssh.GetTypeMeta())
	require.Equal(t, listMeta, ssh.GetListMeta())

	strings := &StringSecretList{}
	strings.SetTypeMeta(typeMeta)
	strings.SetListMeta(listMeta)
	require.Equal(t, typeMeta, strings.GetTypeMeta())
	require.Equal(t, listMeta, strings.GetListMeta())
}

func TestSchemeRegistrationAndDeepCopyIsolation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, SchemeBuilder.AddToScheme(scheme))

	condition := metav1.Condition{Type: ConditionTypeReady, Status: metav1.ConditionTrue, Reason: ReasonReconciled}
	objects := []runtime.Object{
		&BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "basic", Labels: map[string]string{"app": "original"}}, Spec: BasicAuthSpec{Data: map[string]string{"literal": "original"}}, Status: BasicAuthStatus{CommonSecretStatus: CommonSecretStatus{Secret: &corev1.ObjectReference{Name: "basic"}, Conditions: []metav1.Condition{condition}}}},
		&BasicAuthList{Items: []BasicAuth{{Spec: BasicAuthSpec{Data: map[string]string{"literal": "original"}}}}},
		&SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "ssh", Labels: map[string]string{"app": "original"}}, Spec: SSHKeyPairSpec{Data: map[string]string{"literal": "original"}}, Status: SSHKeyPairStatus{CommonSecretStatus: CommonSecretStatus{Secret: &corev1.ObjectReference{Name: "ssh"}, Conditions: []metav1.Condition{condition}}}},
		&SSHKeyPairList{Items: []SSHKeyPair{{Spec: SSHKeyPairSpec{Data: map[string]string{"literal": "original"}}}}},
		&StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "string", Labels: map[string]string{"app": "original"}}, Spec: StringSecretSpec{Data: map[string]string{"literal": "original"}, Fields: []Field{{FieldName: "generated"}}}, Status: StringSecretStatus{CommonSecretStatus: CommonSecretStatus{Secret: &corev1.ObjectReference{Name: "string"}, Conditions: []metav1.Condition{condition}}}},
		&StringSecretList{Items: []StringSecret{{Spec: StringSecretSpec{Data: map[string]string{"literal": "original"}, Fields: []Field{{FieldName: "generated"}}}}}},
	}
	for _, object := range objects {
		gvks, _, err := scheme.ObjectKinds(object)
		require.NoError(t, err)
		require.NotEmpty(t, gvks)
		require.NotSame(t, object, object.DeepCopyObject())
	}

	basic := objects[0].(*BasicAuth)
	basicCopy := basic.DeepCopy()
	basicCopy.Labels["app"] = "copy"
	basicCopy.Spec.Data["literal"] = "copy"
	basicCopy.Status.Secret.Name = "copy"
	basicCopy.Status.Conditions[0].Reason = "copy"
	require.Equal(t, "original", basic.Labels["app"])
	require.Equal(t, "original", basic.Spec.Data["literal"])
	require.Equal(t, "basic", basic.Status.Secret.Name)
	require.Equal(t, ReasonReconciled, basic.Status.Conditions[0].Reason)

	sshList := objects[3].(*SSHKeyPairList)
	sshListCopy := sshList.DeepCopy()
	sshListCopy.Items[0].Spec.Data["literal"] = "copy"
	require.Equal(t, "original", sshList.Items[0].Spec.Data["literal"])

	stringSecret := objects[4].(*StringSecret)
	stringCopy := stringSecret.DeepCopy()
	stringCopy.Spec.Data["literal"] = "copy"
	stringCopy.Spec.Fields[0].FieldName = "copy"
	require.Equal(t, "original", stringSecret.Spec.Data["literal"])
	require.Equal(t, "generated", stringSecret.Spec.Fields[0].FieldName)
}

func TestGeneratedValueDeepCopyHelpers(t *testing.T) {
	require.Nil(t, (*BasicAuth)(nil).DeepCopy())
	require.Nil(t, (*BasicAuthList)(nil).DeepCopy())
	require.Nil(t, (*BasicAuthSpec)(nil).DeepCopy())
	require.Nil(t, (*BasicAuthStatus)(nil).DeepCopy())
	require.Nil(t, (*CommonSecretStatus)(nil).DeepCopy())
	require.Nil(t, (*Field)(nil).DeepCopy())
	require.Nil(t, (*SSHKeyPair)(nil).DeepCopy())
	require.Nil(t, (*SSHKeyPairList)(nil).DeepCopy())
	require.Nil(t, (*SSHKeyPairSpec)(nil).DeepCopy())
	require.Nil(t, (*SSHKeyPairStatus)(nil).DeepCopy())
	require.Nil(t, (*StringSecret)(nil).DeepCopy())
	require.Nil(t, (*StringSecretList)(nil).DeepCopy())
	require.Nil(t, (*StringSecretSpec)(nil).DeepCopy())
	require.Nil(t, (*StringSecretStatus)(nil).DeepCopy())

	require.NotNil(t, (&BasicAuthSpec{}).DeepCopy())
	require.NotNil(t, (&BasicAuthStatus{}).DeepCopy())
	require.NotNil(t, (&CommonSecretStatus{}).DeepCopy())
	require.NotNil(t, (&Field{}).DeepCopy())
	require.NotNil(t, (&SSHKeyPairSpec{}).DeepCopy())
	require.NotNil(t, (&SSHKeyPairStatus{}).DeepCopy())
	require.NotNil(t, (&StringSecretSpec{}).DeepCopy())
	require.NotNil(t, (&StringSecretStatus{}).DeepCopy())

	require.Nil(t, (*BasicAuth)(nil).DeepCopyObject())
	require.Nil(t, (*BasicAuthList)(nil).DeepCopyObject())
	require.Nil(t, (*SSHKeyPair)(nil).DeepCopyObject())
	require.Nil(t, (*SSHKeyPairList)(nil).DeepCopyObject())
	require.Nil(t, (*StringSecret)(nil).DeepCopyObject())
	require.Nil(t, (*StringSecretList)(nil).DeepCopyObject())
}
