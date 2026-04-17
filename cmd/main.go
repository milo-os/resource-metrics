// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcsingle "sigs.k8s.io/multicluster-runtime/providers/single"

	resourcemetricsv1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/collector"
	"go.datum.net/resource-metrics/internal/config"
	"go.datum.net/resource-metrics/internal/controller"
	controllermetrics "go.datum.net/resource-metrics/internal/metrics"
	otelpkg "go.datum.net/resource-metrics/internal/otel"
	"go.datum.net/resource-metrics/internal/policy"
	milomulticluster "go.miloapis.com/milo/pkg/multicluster-runtime"
	miloprovider "go.miloapis.com/milo/pkg/multicluster-runtime/milo"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	codecs   = serializer.NewCodecFactory(scheme, serializer.EnableStrict)
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(resourcemetricsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var serverConfigFile string

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&serverConfigFile, "server-config", "", "path to the server config file")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if len(serverConfigFile) == 0 {
		setupLog.Error(fmt.Errorf("must provide --server-config"), "")
		os.Exit(1)
	}

	var serverConfig config.ResourceMetricsOperator
	data, err := os.ReadFile(serverConfigFile)
	if err != nil {
		setupLog.Error(fmt.Errorf("unable to read server config from %q", serverConfigFile), "")
		os.Exit(1)
	}

	if err := runtime.DecodeInto(codecs.UniversalDecoder(), data, &serverConfig); err != nil {
		setupLog.Error(err, "unable to decode server config")
		os.Exit(1)
	}

	// Fill in defaults for blocks that may have been omitted from the
	// on-disk config (or where the decoder ran non-strict and left zero
	// values for optional fields).
	config.SetDefaults_DiscoveryConfig(&serverConfig.Discovery)
	config.SetDefaults_OtelConfig(&serverConfig.Otel)

	// NOTE: log the String() rendering of the config rather than the struct
	// directly. OtelConfig.Headers commonly carries credentials such as
	// "Authorization: Bearer ..." tokens, and String() redacts those values
	// while preserving header keys.
	setupLog.Info("server config", "config", serverConfig.String())

	runnables, provider, err := initializeClusterDiscovery(serverConfig, scheme)
	if err != nil {
		setupLog.Error(err, "unable to initialize cluster discovery")
		os.Exit(1)
	}

	// Build the CEL environment and the policy registry shared between the
	// ResourceMetricsPolicy reconciler and the per-project collectors.
	celEnv, err := policy.NewEnv()
	if err != nil {
		setupLog.Error(err, "unable to build cel environment")
		os.Exit(1)
	}

	// Wire the controller-runtime counters onto CEL's side-channel hooks so
	// recovered panics and sanitized label values surface as Prometheus
	// counters.
	policy.OnEvalPanic = controllermetrics.DefaultReporter{}.ReportEvalPanic
	policy.OnLabelSanitized = controllermetrics.DefaultReporter{}.ReportLabelSanitized

	registry := policy.NewRegistry(celEnv)
	clusterManager := collector.NewClusterManager(registry, ctrl.Log.WithName("collector"))

	// Build the OTel MeterProvider and the Runtime that owns the
	// per-family observable gauges. The MP is wired here rather than
	// inside the reconciler so that its lifetime matches the operator
	// process: the errgroup below takes responsibility for shutting it
	// down before the collectors/informers stop.
	provisionCtx, provisionCancel := context.WithTimeout(context.Background(), 30*time.Second)
	meterProvider, err := otelpkg.NewMeterProvider(provisionCtx, buildOtelProviderOptions(serverConfig.Otel))
	provisionCancel()
	if err != nil {
		setupLog.Error(err, "unable to build otel meter provider")
		os.Exit(1)
	}

	otelRuntime, err := otelpkg.NewRuntime(
		meterProvider,
		registry,
		otelpkg.NewCollectorSource(clusterManager),
		serverConfig.Otel.DefaultMetricPrefix,
		ctrl.Log.WithName("otel"),
	)
	if err != nil {
		setupLog.Error(err, "unable to build otel runtime")
		os.Exit(1)
	}

	// Wrap the provider so that every cluster the underlying provider engages
	// via mcmanager.Engage also gets forwarded into our ClusterManager. The
	// wrapper handles both the single-cluster "engage on Run" case and the
	// multi-cluster provider case by intercepting the Manager passed to
	// provider.Run.
	provider = wrapProviderWithCollector(provider, clusterManager)

	// ctrl.GetConfigOrDie respects the KUBECONFIG env var and --kubeconfig
	// flag. In milo mode the deployment sets KUBECONFIG=/etc/milo/kubeconfig
	// so the local cluster (and therefore the ResourceMetricsPolicy watch and
	// leader election) targets the Milo control plane.
	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), provider, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "resourcemetrics.miloapis.com",
		LeaderElectionNamespace: "milo-system",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.ResourceMetricsPolicyReconciler{
		Client:         mgr.GetLocalManager().GetClient(),
		Scheme:         scheme,
		Env:            celEnv,
		Registry:       registry,
		ClusterManager: clusterManager,
		OTel:           otelRuntime,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourceMetricsPolicy")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// Readiness verifies that (a) the local manager's cache is synced and
	// (b) the ResourceMetricsPolicy CRD is installed there. In Milo mode the
	// local manager targets the Milo control plane; in single mode it is the
	// GKE cluster. Either missing is a fatal configuration problem we want to
	// surface rather than crashloop on.
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		var list resourcemetricsv1alpha1.ResourceMetricsPolicyList
		if err := mgr.GetLocalManager().GetClient().List(req.Context(), &list, client.Limit(1)); err != nil {
			return fmt.Errorf("resourcemetricspolicy CRD not reachable: %w", err)
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	g, ctx := errgroup.WithContext(ctx)

	for _, runnable := range runnables {
		g.Go(func() error {
			return ignoreCanceled(runnable.Start(ctx))
		})
	}

	setupLog.Info("starting cluster discovery provider")
	g.Go(func() error {
		return ignoreCanceled(provider.Run(ctx, mgr))
	})

	setupLog.Info("starting multicluster manager")
	g.Go(func() error {
		return ignoreCanceled(mgr.Start(ctx))
	})

	// Drain the OTel pipeline before the collectors/informers are torn
	// down. We do this in its own errgroup goroutine so that we can wait
	// for ctx.Done (the signal handler fires) and run a bounded shutdown
	// sequence regardless of how the other goroutines exit.
	g.Go(func() error {
		<-ctx.Done()
		setupLog.Info("shutting down otel runtime")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := otelRuntime.Shutdown(shutdownCtx); err != nil {
			setupLog.Error(err, "otel runtime shutdown")
		}
		if err := meterProvider.Shutdown(shutdownCtx); err != nil {
			setupLog.Error(err, "otel meter provider shutdown")
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		setupLog.Error(err, "unable to start")
		os.Exit(1)
	}
}

// buildOtelProviderOptions turns the on-disk OtelConfig into the narrow
// ProviderOptions shape consumed by otelpkg.NewMeterProvider.
func buildOtelProviderOptions(cfg config.OtelConfig) otelpkg.ProviderOptions {
	attrs := make([]attribute.KeyValue, 0, len(cfg.ResourceAttributes))
	for k, v := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	return otelpkg.ProviderOptions{
		Endpoint:           cfg.Endpoint,
		Insecure:           cfg.Insecure,
		Headers:            cfg.Headers,
		CollectionInterval: cfg.CollectionInterval.Duration,
		ServiceName:        "resource-metrics",
		ResourceAttributes: attrs,
	}
}

type runnableProvider interface {
	multicluster.Provider
	Run(context.Context, mcmanager.Manager) error
}

// wrappedSingleClusterProvider engages the single cluster before delegating
// to the underlying provider's Run.
//
// Needed until we contribute the patch in the following PR again (need to sign CLA):
//
//	See: https://github.com/kubernetes-sigs/multicluster-runtime/pull/18
type wrappedSingleClusterProvider struct {
	multicluster.Provider
	cluster cluster.Cluster
}

func (p *wrappedSingleClusterProvider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	if err := mgr.Engage(ctx, "single", p.cluster); err != nil {
		return err
	}
	return p.Provider.(runnableProvider).Run(ctx, mgr)
}

// engageInterceptor wraps an mcmanager.Manager and forwards every Engage call
// to both the underlying manager and a ClusterManager. This is how we hook
// per-cluster collector creation without modifying upstream providers.
type engageInterceptor struct {
	mcmanager.Manager
	clusterManager *collector.ClusterManager
}

// Engage is called by the provider when a project control plane becomes
// ready. We engage the ClusterManager first; if that fails we do not engage
// the underlying manager so the provider sees the failure atomically.
func (i *engageInterceptor) Engage(ctx context.Context, name string, cl cluster.Cluster) error {
	if err := i.clusterManager.Engage(ctx, name, cl); err != nil {
		return err
	}
	return i.Manager.Engage(ctx, name, cl)
}

// collectorProviderWrapper intercepts the mcmanager.Manager passed to
// provider.Run so that every Engage routed through it is observed by the
// ClusterManager.
type collectorProviderWrapper struct {
	runnableProvider
	clusterManager *collector.ClusterManager
}

func (w *collectorProviderWrapper) Run(ctx context.Context, mgr mcmanager.Manager) error {
	wrapped := &engageInterceptor{Manager: mgr, clusterManager: w.clusterManager}
	return w.runnableProvider.Run(ctx, wrapped)
}

func wrapProviderWithCollector(p runnableProvider, cm *collector.ClusterManager) runnableProvider {
	return &collectorProviderWrapper{runnableProvider: p, clusterManager: cm}
}

func initializeClusterDiscovery(
	serverConfig config.ResourceMetricsOperator,
	scheme *runtime.Scheme,
) (runnables []manager.Runnable, provider runnableProvider, err error) {
	switch serverConfig.Discovery.Mode {
	case milomulticluster.ProviderSingle:
		// In single mode the operator manages exactly one cluster — itself.
		deploymentCluster, err := cluster.New(ctrl.GetConfigOrDie(), func(o *cluster.Options) {
			o.Scheme = scheme
		})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to construct deployment cluster: %w", err)
		}
		runnables = append(runnables, deploymentCluster)
		provider = &wrappedSingleClusterProvider{
			Provider: mcsingle.New("single", deploymentCluster),
			cluster:  deploymentCluster,
		}

	case milomulticluster.ProviderMilo:
		discoveryRestConfig, err := serverConfig.Discovery.DiscoveryRestConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get discovery rest config: %w", err)
		}

		projectRestConfig, err := serverConfig.Discovery.ProjectRestConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get project rest config: %w", err)
		}

		discoveryManager, err := manager.New(discoveryRestConfig, manager.Options{})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to set up discovery manager: %w", err)
		}

		miloProvider, err := miloprovider.New(discoveryManager, miloprovider.Options{
			ClusterOptions: []cluster.Option{
				func(o *cluster.Options) {
					o.Scheme = scheme
				},
			},
			InternalServiceDiscovery: serverConfig.Discovery.InternalServiceDiscovery,
			ProjectRestConfig:        projectRestConfig,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to create milo provider: %w", err)
		}

		provider = miloProvider
		runnables = append(runnables, discoveryManager)

	default:
		return nil, nil, fmt.Errorf(
			"unsupported cluster discovery mode %s",
			serverConfig.Discovery.Mode,
		)
	}

	return runnables, provider, nil
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
