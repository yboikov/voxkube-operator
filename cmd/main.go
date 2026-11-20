package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	voxkubev1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
	"github.com/yboikov/voxkube-operator/internal/controller"
	"github.com/yboikov/voxkube-operator/internal/version"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(voxkubev1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var secureMetrics bool
	var metricsCertDir string
	var probeAddr string
	var enableLeaderElection bool
	var watchNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false, "Serve metrics over HTTPS and require an authenticated, authorized client.")
	flag.StringVar(&metricsCertDir, "metrics-cert-dir", "", "Directory holding tls.crt and tls.key for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "Namespace to restrict the operator to. If empty, watches all namespaces.")

	opts := zap.Options{Development: false}
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "Print the operator version and exit.")
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Println(version.String())
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog.Info("starting voxkube-operator", "version", version.Version, "commit", version.Commit, "buildDate", version.BuildDate,
		"defaultOpenVoxServerImage", controller.DefaultOpenVoxServerImage, "defaultOpenVoxDBImage", controller.DefaultOpenVoxDBImage)

	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions(metricsAddr, secureMetrics, metricsCertDir),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "voxkube-operator.voxkube.io",
	}

	if watchNamespace != "" {
		setupLog.Info("watching single namespace", "namespace", watchNamespace)
		mgrOptions.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				watchNamespace: {},
			},
		}
		mgrOptions.LeaderElectionNamespace = watchNamespace
	} else {
		setupLog.Info("watching all namespaces (cluster-wide)")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Per-VoxNode Puppet run metrics (voxkube_node_*), read from the manager's cache on every scrape.
	crmetrics.Registry.MustRegister(&controller.NodeMetricsCollector{Reader: mgr.GetClient()})

	if err = (&controller.VoxClusterReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorder("voxcluster-controller"),
		GitPoller:      controller.NewSmartHTTPGitPoller(nil),
		TokenExchanger: controller.NewGitHubAppTokenExchanger(nil),
		CAClient:       controller.NewHTTPCAClient(nil),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VoxCluster")
		os.Exit(1)
	}

	if err = (&controller.VoxNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("voxnode-controller"),
		CAClient: controller.NewHTTPCAClient(nil),
		DBClient: controller.NewHTTPPuppetDBClient(nil),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VoxNode")
		os.Exit(1)
	}

	if err = (&controller.VoxDiscoveryReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("voxdiscovery-controller"),
		DBClient: controller.NewHTTPPuppetDBClient(nil),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VoxDiscovery")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting VoxKube manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func metricsOptions(addr string, secure bool, certDir string) metricsserver.Options {
	if !secure {
		return metricsserver.Options{BindAddress: addr}
	}
	opts := metricsserver.Options{
		BindAddress:    addr,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}
	if certDir != "" {
		opts.CertDir = certDir
		opts.CertName = "tls.crt"
		opts.KeyName = "tls.key"
	}
	return opts
}
