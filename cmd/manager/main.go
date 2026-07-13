package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/mittwald/kubernetes-secret-generator/pkg/apis"
	"github.com/mittwald/kubernetes-secret-generator/pkg/controller"
	controllerobservability "github.com/mittwald/kubernetes-secret-generator/pkg/controller/observability"
	secretcontroller "github.com/mittwald/kubernetes-secret-generator/pkg/controller/secret"
	"github.com/mittwald/kubernetes-secret-generator/version"

	"github.com/spf13/pflag"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Change below variables to serve metrics on different host or port.
var (
	metricsHost       = "0.0.0.0"
	metricsPort int32 = 8383
)
var log = logf.Log.WithName("cmd")

func printVersion() {
	log.Info(fmt.Sprintf("Operator Version: %s", version.Version))
	log.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
}

func main() {
	// Add flags registered by imported packages (e.g. glog and
	// controller-runtime)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	pflag.Bool("regenerate-insecure", false, "Set this to automatically regenerate secrets that were generated with an non-cryptographically secure PRNG.")
	pflag.String("secret-length", "40", "Secret length")
	pflag.Int("ssh-key-length", 2048, "Default length of SSH Keys")
	pflag.String("ssh-key-algorithm", "rsa", "Default algorithm for SSH Keys")
	pflag.String("secret-encoding", "base64", "Encoding for secrets")
	pflag.Bool("use-metrics-service", false, "Deprecated; metrics are always exposed on the configured metrics bind address")
	pflag.Bool("disable-crd-support", false, "Whether to disable CRD support and registering")

	pflag.Parse()

	// Import flags into viper and bind them to env vars
	// flags are converted to upper-case, - is replaced with _
	// secret-length -> SECRET_LENGTH
	err := viper.BindPFlags(pflag.CommandLine)
	if err != nil {
		log.Error(err, "failed parsing pflag CommandLine")
		os.Exit(1)
	}

	replacer := strings.NewReplacer("-", "_")
	viper.SetEnvKeyReplacer(replacer)

	viper.AutomaticEnv()

	// Use a zap logr.Logger implementation. If none of the zap
	// flags are configured (or if the zap flag set is not being
	// used), this defaults to a production zap logger.
	//
	// The logger instantiated here can be changed to any logger
	// implementing the logr.Logger interface. This logger will
	// be propagated through the whole operator, generating
	// uniform and structured logs.
	logf.SetLogger(zap.New())
	if err = secretcontroller.ValidateStartupDefaults(); err != nil {
		log.Error(err, "invalid generator startup defaults")
		os.Exit(1)
	}

	printVersion()

	namespaces := watchNamespaces()

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Set default manager options
	options := manager.Options{
		Cache:                  cacheOptions(namespaces),
		Metrics:                metricsserver.Options{BindAddress: fmt.Sprintf("%s:%d", metricsHost, metricsPort)},
		HealthProbeBindAddress: ":8080",
	}

	// add custom resources to scheme
	err = apis.AddToScheme(scheme.Scheme)
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Create a new manager to provide shared dependencies and start components
	var mgr manager.Manager
	mgr, err = manager.New(cfg, options)
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Add liveness probe
	err = mgr.AddHealthzCheck("health-ping", healthz.Ping)
	if err != nil {
		log.Error(err, "couldn't add liveness probe")
		os.Exit(1)
	}

	// Add readiness probe
	err = mgr.AddReadyzCheck("ready-ping", healthz.Ping)
	if err != nil {
		log.Error(err, "couldn't add readiness probe")
		os.Exit(1)
	}
	err = mgr.AddReadyzCheck("cache-sync", func(req *http.Request) error {
		if !mgr.GetCache().WaitForCacheSync(req.Context()) {
			return fmt.Errorf("cache is not synchronized")
		}
		return nil
	})
	if err != nil {
		log.Error(err, "couldn't add cache readiness probe")
		os.Exit(1)
	}
	apiProbe, probeErr := controllerobservability.NewAPIConnectivityProbe(cfg)
	if probeErr != nil {
		log.Error(probeErr, "couldn't create API connectivity probe")
		os.Exit(1)
	}
	if err = mgr.Add(apiProbe); err != nil {
		log.Error(err, "couldn't add API connectivity probe")
		os.Exit(1)
	}

	log.Info("Registering Components.")

	if err = apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	// Setup all Controllers
	if err = controller.AddToManager(mgr, !viper.GetBool("disable-crd-support")); err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	if viper.GetBool("use-metrics-service") {
		log.Info("metrics service autoconfiguration is no longer provided by operator-sdk runtime; use the deployment manifests to expose the metrics endpoint")
	}

	log.Info("Starting the Cmd.")

	// Start the Cmd
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Manager exited non-zero")
		os.Exit(1)
	}
}

func watchNamespaces() []string {
	watchNamespace := strings.TrimSpace(os.Getenv("WATCH_NAMESPACE"))
	if watchNamespace == "" {
		return nil
	}

	namespaces := strings.Split(watchNamespace, ",")
	trimmed := namespaces[:0]
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace != "" {
			trimmed = append(trimmed, namespace)
		}
	}
	return trimmed
}

func cacheOptions(namespaces []string) cache.Options {
	if len(namespaces) == 0 {
		return cache.Options{}
	}

	defaultNamespaces := make(map[string]cache.Config, len(namespaces))
	for _, namespace := range namespaces {
		defaultNamespaces[namespace] = cache.Config{}
	}
	return cache.Options{DefaultNamespaces: defaultNamespaces}
}
