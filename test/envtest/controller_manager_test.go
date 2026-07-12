package envtest_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	controllerlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis"
	"github.com/mittwald/kubernetes-secret-generator/pkg/apis/secretgenerator/v1alpha1"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller/crd"
	secretcontroller "github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
)

const eventuallyTimeout = 12 * time.Second

var (
	testClient     client.Client
	testConfigHost string
)

func TestMain(m *testing.M) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("locate envtest source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
		CRDs:                  loadFluxCRDs(repoRoot),
	}
	cfg, err := testEnvironment.Start()
	if err != nil {
		panic(err)
	}
	testConfigHost = cfg.Host
	controllerlog.SetLogger(logr.Discard())

	scheme := clientgoscheme.Scheme
	if err = apis.AddToScheme(scheme); err != nil {
		panic(err)
	}
	viper.Set("secret-length", 40)
	viper.Set("secret-encoding", "base64")
	viper.Set("regenerate-insecure", false)
	viper.Set("ssh-key-length", 2048)
	viper.Set("ssh-key-algorithm", "rsa")

	mgr, err := manager.New(cfg, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		panic(err)
	}
	if err = controller.AddToManager(mgr, true); err != nil {
		panic(err)
	}
	testClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	managerDone := make(chan error, 1)
	go func() { managerDone <- mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		panic("manager cache did not synchronize")
	}

	code := m.Run()
	cancel()
	if err = <-managerDone; err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "manager stopped with error")
		code = 1
	}
	if err = testEnvironment.Stop(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "envtest stopped with error")
		code = 1
	}
	os.Exit(code)
}

func TestEnvtestUsesExplicitDisposableControlPlane(t *testing.T) {
	u, err := url.Parse(testConfigHost)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("envtest API host is not loopback: %q", host)
	}
}

func TestManagerWatchStringSecretLifecycle(t *testing.T) {
	ns := newNamespace(t)
	name := "string-lifecycle"
	object := &v1alpha1.StringSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"managed": "one"}},
		Spec: v1alpha1.StringSecretSpec{
			Data: map[string]string{"literal": "desired"},
			Fields: []v1alpha1.Field{
				{FieldName: "first", Encoding: "base64url", Length: "24"},
				{FieldName: "second", Encoding: "base64url", Length: "24"},
			},
		},
	}
	create(t, object)
	readyStringSecret(t, ns, name, 1)
	initial := getSecret(t, ns, name)
	assertTracking(t, initial)
	assertStableSecret(t, ns, name, initial.ResourceVersion)

	deletedAt := time.Now()
	deleteObject(t, initial)
	recreated := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return s.UID != initial.UID && !bytes.Equal(s.Data["first"], initial.Data["first"])
	})
	assertRecreatedWithinSLO(t, deletedAt, recreated.UID, func() *v1alpha1.CommonSecretStatus {
		return &getStringSecret(t, ns, name).Status.CommonSecretStatus
	})
	if string(recreated.Data["literal"]) != "desired" {
		t.Fatal("literal data was not recreated")
	}

	beforeDrift := recreated.DeepCopy()
	recreated.Data["literal"] = []byte("wrong")
	recreated.Data["first"] = []byte("tampered")
	recreated.Labels["managed"] = "wrong"
	update(t, recreated)
	repaired := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return string(s.Data["literal"]) == "desired" && !bytes.Equal(s.Data["first"], []byte("tampered")) &&
			s.Labels["managed"] == "one"
	})
	if !bytes.Equal(repaired.Data["second"], beforeDrift.Data["second"]) {
		t.Fatal("unaffected generated field rotated")
	}

	object = getStringSecret(t, ns, name)
	object.Spec.ForceRegenerate = true
	update(t, object)
	forced := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return !bytes.Equal(s.Data["first"], repaired.Data["first"]) && !bytes.Equal(s.Data["second"], repaired.Data["second"])
	})
	readyStringSecret(t, ns, name, object.Generation)
	assertStableSecret(t, ns, name, forced.ResourceVersion)

	object = getStringSecret(t, ns, name)
	object.Spec.ForceRegenerate = false
	object.Spec.Fields[0].Length = "30"
	update(t, object)
	fingerprintChanged := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return len(s.Data["first"]) == 30 && !bytes.Equal(s.Data["first"], forced.Data["first"])
	})
	if !bytes.Equal(fingerprintChanged.Data["second"], forced.Data["second"]) {
		t.Fatal("unaffected field rotated after fingerprint change")
	}
	readyStringSecret(t, ns, name, object.Generation)
	assertStableCRStatus(t, getStringSecret(t, ns, name))

	statusVersion := getStringSecret(t, ns, name).ResourceVersion
	fingerprintChanged.Annotations["example.test/user-note"] = "ignored"
	update(t, fingerprintChanged)
	assertStableSecret(t, ns, name, fingerprintChanged.ResourceVersion)
	time.Sleep(400 * time.Millisecond)
	if got := getStringSecret(t, ns, name).ResourceVersion; got != statusVersion {
		t.Fatalf("unrelated annotation update changed CR status resourceVersion: %s -> %s", statusVersion, got)
	}
}

func TestManagerWatchBasicAuthLifecycle(t *testing.T) {
	ns := newNamespace(t)
	name := "basic-lifecycle"
	object := &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.BasicAuthSpec{Username: "admin", Encoding: "base64url", Length: "24"}}
	create(t, object)
	readyBasicAuth(t, ns, name, 1)
	initial := getSecret(t, ns, name)
	assertTracking(t, initial)

	deletedAt := time.Now()
	deleteObject(t, initial)
	recreated := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return s.UID != initial.UID && !bytes.Equal(s.Data[secretcontroller.FieldBasicAuthPassword], initial.Data[secretcontroller.FieldBasicAuthPassword])
	})
	assertRecreatedWithinSLO(t, deletedAt, recreated.UID, func() *v1alpha1.CommonSecretStatus {
		return &getBasicAuth(t, ns, name).Status.CommonSecretStatus
	})

	recreated.Data[secretcontroller.FieldBasicAuthPassword] = []byte("tampered")
	update(t, recreated)
	repaired := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return !bytes.Equal(s.Data[secretcontroller.FieldBasicAuthPassword], []byte("tampered")) && !bytes.Equal(s.Data[secretcontroller.FieldBasicAuthIngress], recreated.Data[secretcontroller.FieldBasicAuthIngress])
	})

	object = getBasicAuth(t, ns, name)
	object.Spec.ForceRegenerate = true
	update(t, object)
	forced := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return !bytes.Equal(s.Data[secretcontroller.FieldBasicAuthPassword], repaired.Data[secretcontroller.FieldBasicAuthPassword])
	})
	readyBasicAuth(t, ns, name, object.Generation)
	assertStableSecret(t, ns, name, forced.ResourceVersion)

	object = getBasicAuth(t, ns, name)
	object.Spec.ForceRegenerate = false
	object.Spec.Username = "operator"
	update(t, object)
	changed := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return string(s.Data[secretcontroller.FieldBasicAuthUsername]) == "operator" && !bytes.Equal(s.Data[secretcontroller.FieldBasicAuthPassword], forced.Data[secretcontroller.FieldBasicAuthPassword])
	})
	readyBasicAuth(t, ns, name, object.Generation)
	assertStableSecret(t, ns, name, changed.ResourceVersion)
}

func TestManagerWatchSSHKeyPairLifecycle(t *testing.T) {
	ns := newNamespace(t)
	name := "ssh-lifecycle"
	object := &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519"}}
	create(t, object)
	readySSHKeyPair(t, ns, name, 1)
	initial := getSecret(t, ns, name)
	assertTracking(t, initial)

	deletedAt := time.Now()
	deleteObject(t, initial)
	recreated := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return s.UID != initial.UID && !bytes.Equal(s.Data["ssh-privatekey"], initial.Data["ssh-privatekey"])
	})
	assertRecreatedWithinSLO(t, deletedAt, recreated.UID, func() *v1alpha1.CommonSecretStatus {
		return &getSSHKeyPair(t, ns, name).Status.CommonSecretStatus
	})

	privateBeforePublicDrift := append([]byte(nil), recreated.Data["ssh-privatekey"]...)
	recreated.Data["ssh-publickey"] = []byte("tampered")
	update(t, recreated)
	publicRepaired := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return !bytes.Equal(s.Data["ssh-publickey"], []byte("tampered"))
	})
	if !bytes.Equal(publicRepaired.Data["ssh-privatekey"], privateBeforePublicDrift) {
		t.Fatal("private key rotated while repairing public key")
	}

	publicRepaired.Data["ssh-privatekey"] = []byte("tampered")
	update(t, publicRepaired)
	privateRepaired := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return !bytes.Equal(s.Data["ssh-privatekey"], []byte("tampered")) && !bytes.Equal(s.Data["ssh-privatekey"], privateBeforePublicDrift)
	})

	object = getSSHKeyPair(t, ns, name)
	object.Spec.ForceRegenerate = true
	update(t, object)
	forced := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		return !bytes.Equal(s.Data["ssh-privatekey"], privateRepaired.Data["ssh-privatekey"])
	})
	readySSHKeyPair(t, ns, name, object.Generation)
	assertStableSecret(t, ns, name, forced.ResourceVersion)

	object = getSSHKeyPair(t, ns, name)
	object.Spec.ForceRegenerate = false
	object.Spec.PublicKeyField = "public-v2"
	update(t, object)
	changed := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
		_, oldExists := s.Data["ssh-publickey"]
		return !oldExists && len(s.Data["public-v2"]) > 0 && !bytes.Equal(s.Data["ssh-privatekey"], forced.Data["ssh-privatekey"])
	})
	readySSHKeyPair(t, ns, name, object.Generation)
	assertStableSecret(t, ns, name, changed.ResourceVersion)
}

func TestManagerRejectsWrongControllerOwners(t *testing.T) {
	ns := newNamespace(t)
	tests := []struct {
		name   string
		kind   string
		make   func(string) client.Object
		status func(*testing.T, string, string) *v1alpha1.CommonSecretStatus
	}{
		{"string", "StringSecret", func(name string) client.Object {
			return &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value"}}}}
		}, func(t *testing.T, namespace, name string) *v1alpha1.CommonSecretStatus {
			return &getStringSecret(t, namespace, name).Status.CommonSecretStatus
		}},
		{"basic", "BasicAuth", func(name string) client.Object {
			return &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		}, func(t *testing.T, namespace, name string) *v1alpha1.CommonSecretStatus {
			return &getBasicAuth(t, namespace, name).Status.CommonSecretStatus
		}},
		{"ssh", "SSHKeyPair", func(name string) client.Object {
			return &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519"}}
		}, func(t *testing.T, namespace, name string) *v1alpha1.CommonSecretStatus {
			return &getSSHKeyPair(t, namespace, name).Status.CommonSecretStatus
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := "wrong-owner-" + tt.name
			controllerRef := true
			foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: tt.kind, Name: name, UID: "old-uid", Controller: &controllerRef,
			}}}, Data: map[string][]byte{"preserved": []byte("sentinel")}}
			create(t, foreign)
			before := getSecret(t, ns, name)
			create(t, tt.make(name))
			status := waitStatus(t, func() *v1alpha1.CommonSecretStatus { return tt.status(t, ns, name) }, metav1.ConditionFalse, v1alpha1.ReasonSecretOwnershipConflict)
			if status.ObservedGeneration != 1 {
				t.Fatalf("observedGeneration = %d, want 1", status.ObservedGeneration)
			}
			after := getSecret(t, ns, name)
			if after.ResourceVersion != before.ResourceVersion || string(after.Data["preserved"]) != "sentinel" {
				t.Fatal("foreign Secret was modified")
			}
			waitEventCount(t, ns, name, v1alpha1.ReasonSecretOwnershipConflict, 1)
		})
	}
}

func TestManagerTrackingImmutableAndSizeConditions(t *testing.T) {
	ns := newNamespace(t)

	t.Run("checksum-only loss rotates affected field", func(t *testing.T) {
		name := "checksum-loss"
		create(t, newTwoFieldStringSecret(ns, name))
		readyStringSecret(t, ns, name, 1)
		before := getSecret(t, ns, name)
		checksums := map[string]string{}
		if err := json.Unmarshal([]byte(before.Annotations[crd.AnnotationManagedDataChecksums]), &checksums); err != nil {
			t.Fatal(err)
		}
		delete(checksums, "first")
		encoded, err := json.Marshal(checksums)
		if err != nil {
			t.Fatal(err)
		}
		before.Annotations[crd.AnnotationManagedDataChecksums] = string(encoded)
		update(t, before)
		after := waitSecret(t, ns, name, func(s *corev1.Secret) bool {
			return !bytes.Equal(s.Data["first"], before.Data["first"])
		})
		if !bytes.Equal(after.Data["second"], before.Data["second"]) {
			t.Fatal("unaffected field rotated after checksum loss")
		}
		readyStringSecret(t, ns, name, 1)
	})

	t.Run("partial tracking is terminal", func(t *testing.T) {
		name := "partial-tracking"
		create(t, newTwoFieldStringSecret(ns, name))
		readyStringSecret(t, ns, name, 1)
		object := getSecret(t, ns, name)
		delete(object.Annotations, crd.AnnotationManagedDataKeys)
		update(t, object)
		mutated := getSecret(t, ns, name)
		waitStringCondition(t, ns, name, metav1.ConditionFalse, v1alpha1.ReasonTrackingStateConflict)
		assertStableSecret(t, ns, name, mutated.ResourceVersion)
	})

	t.Run("immutable drift is terminal", func(t *testing.T) {
		name := "immutable"
		instance := newTwoFieldStringSecret(ns, name)
		instance.Labels = map[string]string{"managed": "one"}
		create(t, instance)
		readyStringSecret(t, ns, name, 1)
		object := getSecret(t, ns, name)
		immutable := true
		object.Immutable = &immutable
		object.Data["first"] = []byte("tampered")
		object.Labels["managed"] = "wrong"
		update(t, object)
		mutated := getSecret(t, ns, name)
		waitStringCondition(t, ns, name, metav1.ConditionFalse, v1alpha1.ReasonImmutableSecretConflict)
		assertStableSecret(t, ns, name, mutated.ResourceVersion)

		deletedAt := time.Now()
		deleteObject(t, mutated)
		recreated := waitSecret(t, ns, name, func(s *corev1.Secret) bool { return s.UID != mutated.UID })
		assertRecreatedWithinSLO(t, deletedAt, recreated.UID, func() *v1alpha1.CommonSecretStatus {
			return &getStringSecret(t, ns, name).Status.CommonSecretStatus
		})
	})

	t.Run("immutable BasicAuth force request is terminal", func(t *testing.T) {
		name := "immutable-basic-force"
		create(t, &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.BasicAuthSpec{Username: "admin", Length: "24", Encoding: "base64url"}})
		readyBasicAuth(t, ns, name, 1)
		managed := getSecret(t, ns, name)
		immutable := true
		managed.Immutable = &immutable
		update(t, managed)
		managed = getSecret(t, ns, name)
		instance := getBasicAuth(t, ns, name)
		instance.Spec.ForceRegenerate = true
		update(t, instance)
		waitStatus(t, func() *v1alpha1.CommonSecretStatus {
			return &getBasicAuth(t, ns, name).Status.CommonSecretStatus
		}, metav1.ConditionFalse, v1alpha1.ReasonImmutableSecretConflict, instance.Generation)
		assertStableSecret(t, ns, name, managed.ResourceVersion)
	})

	t.Run("immutable SSH fingerprint request is terminal", func(t *testing.T) {
		name := "immutable-ssh-fingerprint"
		create(t, &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519"}})
		readySSHKeyPair(t, ns, name, 1)
		managed := getSecret(t, ns, name)
		immutable := true
		managed.Immutable = &immutable
		update(t, managed)
		managed = getSecret(t, ns, name)
		instance := getSSHKeyPair(t, ns, name)
		instance.Spec.PublicKeyField = "public-v2"
		update(t, instance)
		waitStatus(t, func() *v1alpha1.CommonSecretStatus {
			return &getSSHKeyPair(t, ns, name).Status.CommonSecretStatus
		}, metav1.ConditionFalse, v1alpha1.ReasonImmutableSecretConflict, instance.Generation)
		assertStableSecret(t, ns, name, managed.ResourceVersion)
	})

	t.Run("owner removal enqueues old controller owner", func(t *testing.T) {
		name := "owner-removal"
		create(t, newTwoFieldStringSecret(ns, name))
		readyStringSecret(t, ns, name, 1)
		object := getSecret(t, ns, name)
		object.OwnerReferences = nil
		update(t, object)
		mutated := getSecret(t, ns, name)
		waitStringCondition(t, ns, name, metav1.ConditionFalse, v1alpha1.ReasonSecretOwnershipConflict)
		assertStableSecret(t, ns, name, mutated.ResourceVersion)
	})

	t.Run("projected size is terminal", func(t *testing.T) {
		name := "oversized"
		create(t, newTwoFieldStringSecret(ns, name))
		readyStringSecret(t, ns, name, 1)
		object := getSecret(t, ns, name)
		object.Data["unmanaged-padding"] = bytes.Repeat([]byte("x"), 590*1024)
		update(t, object)
		mutated := getSecret(t, ns, name)
		waitStringCondition(t, ns, name, metav1.ConditionFalse, v1alpha1.ReasonSecretSizeConflict)
		assertStableSecret(t, ns, name, mutated.ResourceVersion)
	})
}

func TestManagerTerminalInvalidSpecDoesNotRetry(t *testing.T) {
	ns := newNamespace(t)
	name := "invalid-private-key"
	object := &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{
		Algorithm: "ed25519", PrivateKey: "not-a-private-key",
	}}
	create(t, object)
	waitStatus(t, func() *v1alpha1.CommonSecretStatus {
		return &getSSHKeyPair(t, ns, name).Status.CommonSecretStatus
	}, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec, 1)
	stable := getSSHKeyPair(t, ns, name).ResourceVersion
	waitEventCount(t, ns, name, v1alpha1.ReasonInvalidSpec, 1)
	time.Sleep(400 * time.Millisecond)
	if got := getSSHKeyPair(t, ns, name).ResourceVersion; got != stable {
		t.Fatalf("terminal invalid spec retried status writes: %s -> %s", stable, got)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid spec Secret get error = %v, want NotFound", err)
	}
}

func TestCRDAdmissionAndExamples(t *testing.T) {
	ns := newNamespace(t)
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	examples, err := filepath.Glob(filepath.Join(filepath.Dir(filename), "..", "..", "deploy", "cr-examples", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) != 3 {
		t.Fatalf("example count = %d, want 3", len(examples))
	}
	for _, path := range examples {
		t.Run(filepath.Base(path), func(t *testing.T) {
			manifest, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			jsonManifest, err := yaml.YAMLToJSON(manifest)
			if err != nil {
				t.Fatal(err)
			}
			object, _, err := clientgoscheme.Codecs.UniversalDeserializer().Decode(jsonManifest, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			resource, ok := object.(client.Object)
			if !ok {
				t.Fatalf("decoded %T is not a client object", object)
			}
			resource.SetNamespace(ns)
			if err = testClient.Create(context.Background(), resource, client.DryRunAll); err != nil {
				t.Fatalf("example rejected by server-side dry-run: %v", err)
			}
			if err = testClient.Create(context.Background(), resource); err != nil {
				t.Fatalf("example rejected by admission: %v", err)
			}
		})
	}

	invalid := []client.Object{
		&v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "invalid-length", Namespace: ns}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value", Length: "0"}}}},
		&v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "invalid-string-interval", Namespace: ns}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value"}}, RotationInterval: "not-a-duration"}},
		&v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "invalid-literal-only-interval", Namespace: ns}, Spec: v1alpha1.StringSecretSpec{Data: map[string]string{"literal": "value"}, RotationInterval: "1h"}},
		&v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "invalid-username", Namespace: ns}, Spec: v1alpha1.BasicAuthSpec{Username: "bad:name"}},
		&v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "invalid-short-interval", Namespace: ns}, Spec: v1alpha1.BasicAuthSpec{RotationInterval: "59s"}},
		&v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "invalid-zero-interval", Namespace: ns}, Spec: v1alpha1.BasicAuthSpec{RotationInterval: "0s"}},
		&v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "invalid-negative-interval", Namespace: ns}, Spec: v1alpha1.BasicAuthSpec{RotationInterval: "-1m"}},
		&v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "invalid-algorithm", Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "dsa"}},
		&v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "invalid-long-interval", Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", RotationInterval: "8761h"}},
		&v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "invalid-supplied-key-interval", Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", PrivateKey: "supplied", RotationInterval: "1h"}},
	}
	for _, object := range invalid {
		if err := testClient.Create(context.Background(), object); err == nil || !apierrors.IsInvalid(err) {
			t.Fatalf("invalid %T admission error = %v", object, err)
		}
	}
	if err := testClient.Create(context.Background(), &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "ed25519-ignored-length", Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Length: "65537"}}, client.DryRunAll); err != nil {
		t.Fatalf("Ed25519 ignored length was rejected: %v", err)
	}

	for _, tt := range []struct {
		name   string
		object func(map[string]string) client.Object
		max    int
	}{
		{name: "BasicAuth", max: 253, object: func(data map[string]string) client.Object {
			return &v1alpha1.BasicAuth{ObjectMeta: metav1.ObjectMeta{Name: "basic-data-boundary", Namespace: ns}, Spec: v1alpha1.BasicAuthSpec{Data: data}}
		}},
		{name: "SSHKeyPair", max: 254, object: func(data map[string]string) client.Object {
			return &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: "ssh-data-boundary", Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Algorithm: "ed25519", Data: data}}
		}},
	} {
		t.Run(tt.name+" managed key admission boundary", func(t *testing.T) {
			data := make(map[string]string, tt.max+1)
			for i := 0; i < tt.max; i++ {
				data[fmt.Sprintf("literal-%03d", i)] = "value"
			}
			if err := testClient.Create(context.Background(), tt.object(data), client.DryRunAll); err != nil {
				t.Fatalf("maximum literal key count rejected: %v", err)
			}
			data["literal-overflow"] = "value"
			if err := testClient.Create(context.Background(), tt.object(data), client.DryRunAll); err == nil || !apierrors.IsInvalid(err) {
				t.Fatalf("overflow literal key count error = %v, want Invalid", err)
			}
		})
	}
}

func TestSecretTypeCreationAndKubernetesImmutability(t *testing.T) {
	ns := newNamespace(t)
	stringObject := &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: "creation-type", Namespace: ns}, Spec: v1alpha1.StringSecretSpec{
		Type: "example.test/one", Fields: []v1alpha1.Field{{FieldName: "value"}},
	}}
	create(t, stringObject)
	readyStringSecret(t, ns, stringObject.Name, 1)
	managed := getSecret(t, ns, stringObject.Name)
	if managed.Type != corev1.SecretType("example.test/one") {
		t.Fatalf("created Secret type = %q", managed.Type)
	}
	target := managed.DeepCopy()
	target.Type = "example.test/two"
	wrote, err := (&crd.Client{Client: testClient}).PatchSecret(context.Background(), managed, target)
	if wrote || err == nil {
		t.Fatalf("defensive type patch result = (%v, %v)", wrote, err)
	}
	if err = testClient.Patch(context.Background(), target, client.MergeFrom(managed)); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("Kubernetes direct Secret type patch error = %v, want Invalid", err)
	}
}

func TestCRDSecretTypeSemanticImmutability(t *testing.T) {
	ns := newNamespace(t)
	for _, kind := range []string{"string", "ssh"} {
		for i, tt := range []struct {
			old, next string
			allowed   bool
		}{
			{old: "", next: "Opaque", allowed: true},
			{old: "Opaque", next: "", allowed: true},
			{old: "example.test/one", next: "example.test/one", allowed: true},
			{old: "example.test/one", next: "example.test/two"},
			{old: "Opaque", next: "example.test/one"},
			{old: "example.test/one", next: ""},
		} {
			name := fmt.Sprintf("%s-type-%d", kind, i)
			var object client.Object
			if kind == "string" {
				object = &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.StringSecretSpec{Type: tt.old, Fields: []v1alpha1.Field{{FieldName: "value"}}}}
			} else {
				object = &v1alpha1.SSHKeyPair{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.SSHKeyPairSpec{Type: tt.old, Algorithm: "ed25519"}}
			}
			create(t, object)
			if kind == "string" {
				readyStringSecret(t, ns, name, 1)
				current := getStringSecret(t, ns, name)
				current.Spec.Type, current.Spec.ForceRegenerate = tt.next, !current.Spec.ForceRegenerate
				object = current
			} else {
				readySSHKeyPair(t, ns, name, 1)
				current := getSSHKeyPair(t, ns, name)
				current.Spec.Type, current.Spec.ForceRegenerate = tt.next, !current.Spec.ForceRegenerate
				object = current
			}
			err := testClient.Update(context.Background(), object)
			if tt.allowed && err != nil {
				t.Fatalf("%s %q -> %q rejected: %v", kind, tt.old, tt.next, err)
			}
			if !tt.allowed && (err == nil || !apierrors.IsInvalid(err)) {
				t.Fatalf("%s %q -> %q error = %v, want Invalid", kind, tt.old, tt.next, err)
			}
		}
	}
}

func TestRuntimeSecretTypeMismatchIsTerminalAndTransitionDeduplicated(t *testing.T) {
	ns := newNamespace(t)
	name := "runtime-type-mismatch"
	create(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Type: "example.test/actual", Data: map[string][]byte{"value": []byte("preserved")}})
	create(t, &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.StringSecretSpec{Type: "example.test/desired", Fields: []v1alpha1.Field{{FieldName: "value"}}}})
	waitStringCondition(t, ns, name, metav1.ConditionFalse, v1alpha1.ReasonSecretOwnershipConflict)
	instance := getStringSecret(t, ns, name)
	managed := getSecret(t, ns, name)
	controller := true
	managed.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: "StringSecret", Name: name, UID: instance.UID, Controller: &controller}}
	update(t, managed)
	managed = getSecret(t, ns, name)
	waitStringCondition(t, ns, name, metav1.ConditionFalse, v1alpha1.ReasonSecretTypeConflict)
	assertStableSecret(t, ns, name, managed.ResourceVersion)
	waitEventCount(t, ns, name, v1alpha1.ReasonSecretTypeConflict, 1)
}

func TestCRLabelChangeAndRemovalConvergesWithoutGenerationChange(t *testing.T) {
	ns := newNamespace(t)
	name := "label-only-update"
	create(t, &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"managed": "one"}}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{{FieldName: "value"}}}})
	readyStringSecret(t, ns, name, 1)
	object := getStringSecret(t, ns, name)
	generation := object.Generation
	object.Labels["managed"] = "two"
	update(t, object)
	waitSecret(t, ns, name, func(s *corev1.Secret) bool { return s.Labels["managed"] == "two" })
	object = getStringSecret(t, ns, name)
	delete(object.Labels, "managed")
	update(t, object)
	waitSecret(t, ns, name, func(s *corev1.Secret) bool { _, ok := s.Labels["managed"]; return !ok })
	if got := getStringSecret(t, ns, name).Generation; got != generation {
		t.Fatalf("label-only update changed generation: %d -> %d", generation, got)
	}
}

func TestDocumentationExamplesServerDryRun(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	entries, err := documentationExampleIndex(filepath.Join(repoRoot, "docs", "examples", "index.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"application", "flux-system", "docs-example-system"} {
		createDocumentationNamespace(t, namespace)
	}
	for _, entry := range entries {
		path := filepath.Join(repoRoot, filepath.FromSlash(entry.path))
		switch entry.validator {
		case "kubernetes-admission", "flux":
			dryRunManifestFile(t, path)
		case "helm-values":
			helm := os.Getenv("DOC_EXAMPLES_HELM")
			if helm == "" {
				t.Skip("DOC_EXAMPLES_HELM is required for the dedicated documentation gate")
			}
			command := exec.Command(helm, "template", "docs-example", filepath.Join(repoRoot, "deploy", "helm-chart", "kubernetes-secret-generator"), "--namespace", "docs-example-system", "--values", path)
			var rendered, diagnostics bytes.Buffer
			command.Stdout, command.Stderr = &rendered, &diagnostics
			if err := command.Run(); err != nil {
				t.Fatalf("locked Helm schema/render rejected %s: %v: %s", entry.path, err, diagnostics.String())
			}
			dryRunManifest(t, entry.path+" rendered objects", bytes.NewReader(rendered.Bytes()), "docs-example-system")
		default:
			t.Fatalf("unknown indexed validator %q", entry.validator)
		}
	}
}

type documentationExample struct{ path, validator string }

func documentationExampleIndex(path string) ([]documentationExample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []documentationExample
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed documentation example index")
		}
		entries = append(entries, documentationExample{parts[0], parts[1]})
	}
	return entries, nil
}

func createDocumentationNamespace(t *testing.T, name string) {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"ksg-test-owner": "documentation"}}}
	create(t, namespace)
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), namespace) })
}

func dryRunManifestFile(t *testing.T, path string) {
	t.Helper()
	manifest, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()
	dryRunManifest(t, path, manifest, "")
}

func dryRunManifest(t *testing.T, label string, manifest io.Reader, effectiveNamespace string) {
	t.Helper()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(manifest))
	count := 0
	for {
		document, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(document)) == 0 {
			continue
		}
		jsonDocument, err := yaml.YAMLToJSON(document)
		if err != nil {
			t.Fatal(err)
		}
		object := &unstructured.Unstructured{}
		if _, _, err = unstructured.UnstructuredJSONScheme.Decode(jsonDocument, nil, object); err != nil {
			t.Fatal(err)
		}
		mapping, mappingErr := testClient.RESTMapper().RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
		if mappingErr == nil && mapping.Scope.Name() == "namespace" {
			if object.GetNamespace() == "" {
				if effectiveNamespace == "" {
					t.Fatalf("%s object %s/%s omits its documented namespace", label, object.GetKind(), object.GetName())
				}
				object.SetNamespace(effectiveNamespace)
			} else if effectiveNamespace != "" && object.GetNamespace() != effectiveNamespace {
				t.Fatalf("%s object %s/%s conflicts with Helm release namespace", label, object.GetKind(), object.GetName())
			}
		}
		if err = testClient.Create(context.Background(), object, client.DryRunAll); err != nil {
			t.Fatalf("%s rejected by server-side dry-run: %v", label, err)
		}
		count++
	}
	if count == 0 {
		t.Fatalf("%s contains no objects", label)
	}
}

func loadFluxCRDs(repoRoot string) []*extensionsv1.CustomResourceDefinition {
	fixtures := []struct{ path, digest, name, version string }{
		{"source.toolkit.fluxcd.io_gitrepositories.yaml.gz", "6410448fe8853182dcfb54bff5ee7bafd88955cc8e6fd3759be2202fb6541fc2", "gitrepositories.source.toolkit.fluxcd.io", "v1"},
		{"helm.toolkit.fluxcd.io_helmreleases.yaml.gz", "5cc5f6909c4edc767aceade6a799ffbe7d7d5c14c7e557f6c2c44f82ba54f486", "helmreleases.helm.toolkit.fluxcd.io", "v2"},
	}
	var out []*extensionsv1.CustomResourceDefinition
	for _, fixture := range fixtures {
		path := filepath.Join(repoRoot, "test", "fixtures", "flux", "v2.4.0", fixture.path)
		crd, err := loadFluxCRD(path, fixture.digest, fixture.name, fixture.version)
		if err != nil {
			panic(err)
		}
		out = append(out, crd)
	}
	return out
}

func loadFluxCRD(path, expectedDigest, expectedName, expectedVersion string) (*extensionsv1.CustomResourceDefinition, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Flux CRD fixture must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer compressed.Close()
	data, err := io.ReadAll(io.LimitReader(compressed, 4<<20))
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != expectedDigest {
		return nil, errors.New("Flux CRD fixture digest mismatch")
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	crd := &extensionsv1.CustomResourceDefinition{}
	if err = json.Unmarshal(jsonData, crd); err != nil || crd.Name != expectedName {
		return nil, errors.New("Flux CRD fixture identity mismatch")
	}
	for _, version := range crd.Spec.Versions {
		if version.Name == expectedVersion && version.Served && version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			return crd, nil
		}
	}
	return nil, errors.New("Flux CRD fixture target schema missing")
}

func TestFluxCRDFixtureRejectsTamperAndMissingTargetSchema(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(filename), "..", "fixtures", "flux", "v2.4.0", "source.toolkit.fluxcd.io_gitrepositories.yaml.gz")
	if _, err := loadFluxCRD(path, strings.Repeat("0", 64), "gitrepositories.source.toolkit.fluxcd.io", "v1"); err == nil {
		t.Fatal("tampered Flux fixture digest accepted")
	}
	if _, err := loadFluxCRD(path, "6410448fe8853182dcfb54bff5ee7bafd88955cc8e6fd3759be2202fb6541fc2", "gitrepositories.source.toolkit.fluxcd.io", "v999"); err == nil {
		t.Fatal("Flux fixture without target served schema accepted")
	}
}

func newTwoFieldStringSecret(namespace, name string) *v1alpha1.StringSecret {
	return &v1alpha1.StringSecret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: v1alpha1.StringSecretSpec{Fields: []v1alpha1.Field{
		{FieldName: "first", Encoding: "base64url", Length: "24"},
		{FieldName: "second", Encoding: "base64url", Length: "24"},
	}}}
}

func newNamespace(t *testing.T) string {
	t.Helper()
	name := "ksg-envtest-" + strings.ToLower(uuid.NewString())[:12]
	create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"ksg-test-owner": name}}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = testClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return name
}

func create(t *testing.T, object client.Object) {
	t.Helper()
	if err := testClient.Create(context.Background(), object); err != nil {
		t.Fatalf("create %T: %v", object, err)
	}
}

func update(t *testing.T, object client.Object) {
	t.Helper()
	if err := testClient.Update(context.Background(), object); err != nil {
		t.Fatalf("update %T: %v", object, err)
	}
}

func deleteObject(t *testing.T, object client.Object) {
	t.Helper()
	if err := testClient.Delete(context.Background(), object); err != nil {
		t.Fatalf("delete %T: %v", object, err)
	}
}

func getSecret(t *testing.T, namespace, name string) *corev1.Secret {
	t.Helper()
	object := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		t.Fatalf("get Secret: %v", err)
	}
	return object
}

func waitSecret(t *testing.T, namespace, name string, predicate func(*corev1.Secret) bool) *corev1.Secret {
	t.Helper()
	var found *corev1.Secret
	eventually(t, func() error {
		object := &corev1.Secret{}
		if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
			return err
		}
		if !predicate(object) {
			return fmt.Errorf("Secret has not converged")
		}
		found = object
		return nil
	})
	return found
}

func assertTracking(t *testing.T, object *corev1.Secret) {
	t.Helper()
	if _, state, err := crd.LoadTracking(object); err != nil || state != crd.TrackingValid {
		t.Fatalf("tracking state = %v, error = %v", state, err)
	}
}

func assertRecreatedWithinSLO(t *testing.T, started time.Time, uid types.UID, status func() *v1alpha1.CommonSecretStatus) {
	t.Helper()
	eventually(t, func() error {
		ref := status().Secret
		if ref == nil || ref.UID != uid {
			return fmt.Errorf("status Secret UID has not converged")
		}
		return nil
	})
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("Secret recreation and status convergence took %s, want <= 10s", elapsed)
	}
}

func assertStableSecret(t *testing.T, namespace, name, resourceVersion string) {
	t.Helper()
	time.Sleep(400 * time.Millisecond)
	if got := getSecret(t, namespace, name).ResourceVersion; got != resourceVersion {
		t.Fatalf("Secret resourceVersion changed after convergence: %s -> %s", resourceVersion, got)
	}
}

func assertStableCRStatus(t *testing.T, object *v1alpha1.StringSecret) {
	t.Helper()
	time.Sleep(400 * time.Millisecond)
	got := getStringSecret(t, object.Namespace, object.Name)
	if got.ResourceVersion != object.ResourceVersion {
		t.Fatalf("CR resourceVersion changed after convergence: %s -> %s", object.ResourceVersion, got.ResourceVersion)
	}
}

func readyStringSecret(t *testing.T, namespace, name string, generation int64) *v1alpha1.CommonSecretStatus {
	t.Helper()
	return waitStatus(t, func() *v1alpha1.CommonSecretStatus {
		object := getStringSecret(t, namespace, name)
		return &object.Status.CommonSecretStatus
	}, metav1.ConditionTrue, v1alpha1.ReasonReconciled, generation)
}

func readyBasicAuth(t *testing.T, namespace, name string, generation int64) *v1alpha1.CommonSecretStatus {
	t.Helper()
	return waitStatus(t, func() *v1alpha1.CommonSecretStatus {
		object := getBasicAuth(t, namespace, name)
		return &object.Status.CommonSecretStatus
	}, metav1.ConditionTrue, v1alpha1.ReasonReconciled, generation)
}

func readySSHKeyPair(t *testing.T, namespace, name string, generation int64) *v1alpha1.CommonSecretStatus {
	t.Helper()
	return waitStatus(t, func() *v1alpha1.CommonSecretStatus {
		object := getSSHKeyPair(t, namespace, name)
		return &object.Status.CommonSecretStatus
	}, metav1.ConditionTrue, v1alpha1.ReasonReconciled, generation)
}

func waitStringCondition(t *testing.T, namespace, name string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	waitStatus(t, func() *v1alpha1.CommonSecretStatus {
		object := getStringSecret(t, namespace, name)
		return &object.Status.CommonSecretStatus
	}, status, reason, 1)
}

func waitStatus(t *testing.T, get func() *v1alpha1.CommonSecretStatus, status metav1.ConditionStatus, reason string, generation ...int64) *v1alpha1.CommonSecretStatus {
	t.Helper()
	var found *v1alpha1.CommonSecretStatus
	eventually(t, func() error {
		current := get()
		condition := apiMeta.FindStatusCondition(current.Conditions, v1alpha1.ConditionTypeReady)
		if condition == nil || condition.Status != status || condition.Reason != reason {
			return fmt.Errorf("Ready condition has not converged")
		}
		if len(generation) > 0 && current.ObservedGeneration != generation[0] {
			return fmt.Errorf("observedGeneration has not converged")
		}
		found = current
		return nil
	})
	return found
}

func getStringSecret(t *testing.T, namespace, name string) *v1alpha1.StringSecret {
	t.Helper()
	object := &v1alpha1.StringSecret{}
	get(t, namespace, name, object)
	return object
}

func getBasicAuth(t *testing.T, namespace, name string) *v1alpha1.BasicAuth {
	t.Helper()
	object := &v1alpha1.BasicAuth{}
	get(t, namespace, name, object)
	return object
}

func getSSHKeyPair(t *testing.T, namespace, name string) *v1alpha1.SSHKeyPair {
	t.Helper()
	object := &v1alpha1.SSHKeyPair{}
	get(t, namespace, name, object)
	return object
}

func get(t *testing.T, namespace, name string, object client.Object) {
	t.Helper()
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		t.Fatalf("get %T: %v", object, err)
	}
}

func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(eventuallyTimeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out: %v", err)
}

func waitEventCount(t *testing.T, namespace, name, reason string, want int32) {
	t.Helper()
	eventually(t, func() error {
		events := &corev1.EventList{}
		if err := testClient.List(context.Background(), events, client.InNamespace(namespace)); err != nil {
			return err
		}
		var count int32
		for i := range events.Items {
			event := &events.Items[i]
			if event.InvolvedObject.Name == name && event.Reason == reason {
				if strings.Contains(event.Message, "KSG_TEST_SECRET") || strings.Contains(event.Message, "managed-data-checksums") {
					return fmt.Errorf("Event contains sensitive material")
				}
				if event.Count == 0 {
					count++
				} else {
					count += event.Count
				}
			}
		}
		if count != want {
			return fmt.Errorf("Event count = %d, want %d", count, want)
		}
		return nil
	})
}

// Keep the expected API identity close to the manager test so schema drift is
// caught even if an example happens to omit TypeMeta during construction.
func TestCRDGroupVersion(t *testing.T) {
	want := schema.GroupVersion{Group: "secretgenerator.mittwald.de", Version: "v1alpha1"}
	if v1alpha1.SchemeGroupVersion != want {
		t.Fatalf("group version = %s, want %s", v1alpha1.SchemeGroupVersion, want)
	}
}
