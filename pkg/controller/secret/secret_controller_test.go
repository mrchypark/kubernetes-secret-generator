package secret_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
)

var mgr manager.Manager

const labelSecretGeneratorTest = "kubernetes-secret-generator-test"

func getSecretName() string {
	return uuid.New().String()
}

func TestMain(m *testing.M) {
	// Fuzz workers are subprocesses of the coordinator and must not each start
	// a Kubernetes control plane. The coordinator still runs TestMain normally.
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-test.fuzzworker") {
			os.Exit(m.Run())
		}
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("locate test source")
	}
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(filepath.Dir(filename), "..", "..", "..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnvironment.Start()
	if err != nil {
		panic(err)
	}

	mgrOpts := manager.Options{
		NewClient: func(config *rest.Config, options client.Options) (client.Client, error) {
			config.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
			options.Cache = nil
			return client.New(config, options)
		},
	}

	// add custom resources to scheme
	err = apis.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}

	mgr, err = manager.New(cfg, mgrOpts)
	if err != nil {
		panic(err)
	}

	if err = apis.AddToScheme(mgr.GetScheme()); err != nil {
		panic(err)
	}
	setupViper()
	code := m.Run()
	if err = testEnvironment.Stop(); err != nil && code == 0 {
		code = 1
	}

	os.Exit(code)
}

func setupViper() {
	viper.Set("secret-length", 40)
	viper.Set("secret-encoding", "base64")
	viper.Set("regenerate-insecure", false)
	viper.Set("ssh-key-length", 2048)
	viper.Set("ssh-key-algorithm", "rsa")
}

func doReconcile(t *testing.T, targetSecret *corev1.Secret, isErr bool) {
	rec := secret.NewReconciler(mgr)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: targetSecret.Name, Namespace: targetSecret.Namespace}}

	res, err := rec.Reconcile(context.TODO(), req)

	if isErr {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	require.Equal(t, reconcile.Result{}, res)
}

func TestDoesNotTouchOtherSecrets(t *testing.T) {
	secret := &corev1.Secret{
		Type: corev1.SecretTypeOpaque,
		ObjectMeta: metav1.ObjectMeta{
			Name:      getSecretName(),
			Namespace: "default",
			Labels: map[string]string{
				labelSecretGeneratorTest: "yes",
			},
		},
		Data: map[string][]byte{
			"testkey":  []byte("test"),
			"testkey2": []byte("test2"),
		},
	}

	require.NoError(t, mgr.GetClient().Create(context.TODO(), secret))

	doReconcile(t, secret, false)

	out := &corev1.Secret{}
	require.NoError(t, mgr.GetClient().Get(context.TODO(), types.NamespacedName{
		Name:      secret.Name,
		Namespace: secret.Namespace}, out))

	if !reflect.DeepEqual(secret, out) {
		t.Errorf("secret without operator annotations has been reconciled")
	}
}

func TestAnnotationControllerDriftSelfHealAndResourceVersionStability(t *testing.T) {
	instance := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: getSecretName(), Namespace: "default", Annotations: map[string]string{
			secret.AnnotationSecretAutoGenerate: "generated",
		}},
		Data: map[string][]byte{"unmanaged": []byte("preserved")},
	}
	require.NoError(t, mgr.GetClient().Create(context.TODO(), instance))
	doReconcile(t, instance, false)

	key := client.ObjectKeyFromObject(instance)
	generated := &corev1.Secret{}
	require.NoError(t, mgr.GetClient().Get(context.TODO(), key, generated))
	require.NotEmpty(t, generated.Data["generated"])
	if !bytes.Equal([]byte("preserved"), generated.Data["unmanaged"]) {
		t.Fatal("unmanaged Secret data was not preserved")
	}
	if generated.Annotations[secret.AnnotationManagedBy] != secret.AnnotationControllerMarker {
		t.Fatal("generated Secret is missing the controller marker")
	}
	stableVersion := generated.ResourceVersion

	doReconcile(t, generated, false)
	stable := &corev1.Secret{}
	require.NoError(t, mgr.GetClient().Get(context.TODO(), key, stable))
	require.Equal(t, stableVersion, stable.ResourceVersion)

	stable.Data["generated"] = []byte("tampered")
	require.NoError(t, mgr.GetClient().Update(context.TODO(), stable))
	tamperedVersion := stable.ResourceVersion
	doReconcile(t, stable, false)
	repaired := &corev1.Secret{}
	require.NoError(t, mgr.GetClient().Get(context.TODO(), key, repaired))
	if bytes.Equal([]byte("tampered"), repaired.Data["generated"]) {
		t.Fatal("managed Secret data was not repaired")
	}
	if !bytes.Equal([]byte("preserved"), repaired.Data["unmanaged"]) {
		t.Fatal("repair changed unmanaged Secret data")
	}
	require.NotEqual(t, tamperedVersion, repaired.ResourceVersion)

	repairedVersion := repaired.ResourceVersion
	doReconcile(t, repaired, false)
	require.NoError(t, mgr.GetClient().Get(context.TODO(), key, repaired))
	require.Equal(t, repairedVersion, repaired.ResourceVersion)
}

func TestAnnotationControllerImmutableAndInvalidAreTerminalWithoutWrite(t *testing.T) {
	immutable := true
	instance := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: getSecretName(), Namespace: "default", Annotations: map[string]string{
			secret.AnnotationSecretAutoGenerate: "generated",
		}},
		Immutable: &immutable,
		Data:      map[string][]byte{},
	}
	require.NoError(t, mgr.GetClient().Create(context.TODO(), instance))
	version := instance.ResourceVersion
	doReconcile(t, instance, false)
	out := &corev1.Secret{}
	require.NoError(t, mgr.GetClient().Get(context.TODO(), client.ObjectKeyFromObject(instance), out))
	require.Equal(t, version, out.ResourceVersion)
	require.Empty(t, out.Data)
	require.NotContains(t, out.Annotations, secret.AnnotationManagedBy)

	invalid := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getSecretName(), Namespace: "default", Annotations: map[string]string{
		secret.AnnotationSecretAutoGenerate: "generated",
		secret.AnnotationSecretRegenerate:   "unknown",
	}}}
	require.NoError(t, mgr.GetClient().Create(context.TODO(), invalid))
	invalidVersion := invalid.ResourceVersion
	doReconcile(t, invalid, false)
	require.NoError(t, mgr.GetClient().Get(context.TODO(), client.ObjectKeyFromObject(invalid), out))
	require.Equal(t, invalidVersion, out.ResourceVersion)
	if out.Annotations[secret.AnnotationSecretRegenerate] != "unknown" {
		t.Fatal("invalid regenerate request was not retained")
	}
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}
