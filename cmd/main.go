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
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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
	lease := bindLeaseFlags(flag.CommandLine)

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
	// ResourceMetricsPolicy reconciler and the per-control-plane collectors.
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
	// wrapper handles both the single-cluster "engage on Start" case and the
	// multi-cluster provider case by intercepting the Aware passed to
	// provider.Start.
	provider = wrapProviderWithCollector(provider, clusterManager)

	// ctrl.GetConfigOrDie respects the KUBECONFIG env var and --kubeconfig
	// flag. In milo mode the deployment sets KUBECONFIG=/etc/milo/kubeconfig
	// so the local cluster (and therefore the ResourceMetricsPolicy watch and
	// leader election) targets the Milo control plane.
	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), managedProvider{provider},
		managerOptions(metricsAddr, probeAddr, enableLeaderElection, lease))
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

	workers := make([]func(context.Context) error, 0, len(runnables)+2)
	for _, runnable := range runnables {
		workers = append(workers, runnable.Start)
	}
	workers = append(workers,
		func(ctx context.Context) error { return provider.Start(ctx, mgr) },
		shutdownOnStop(otelRuntime, meterProvider),
	)

	setupLog.Info("starting cluster discovery provider and multicluster manager")
	if err := runUntilManagerStops(ctrl.SetupSignalHandler(), mgr.Start, workers...); err != nil {
		setupLog.Error(err, "unable to start")
		os.Exit(1)
	}
}

// runUntilManagerStops runs the manager and the workers until the manager
// returns, a worker fails, or ctx ends, and then stops all of them. A manager
// that returns, for any reason, ends the process, so no replica stays Running
// while it leads nothing.
func runUntilManagerStops(
	ctx context.Context,
	startManager func(context.Context) error,
	workers ...func(context.Context) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)
	for _, work := range workers {
		g.Go(func() error { return ignoreCanceled(work(ctx)) })
	}
	g.Go(func() error {
		defer cancel()
		return ignoreCanceled(startManager(ctx))
	})
	return g.Wait()
}

// otelShutdownTimeout bounds how long the OTel pipeline may take to drain.
const otelShutdownTimeout = 10 * time.Second

type shutdowner interface {
	Shutdown(context.Context) error
}

var (
	_ shutdowner = (*otelpkg.Runtime)(nil)
	_ shutdowner = (*sdkmetric.MeterProvider)(nil)
)

// shutdownOnStop returns a worker that waits for the run to stop, then shuts
// each pipeline stage down in order, within otelShutdownTimeout.
func shutdownOnStop(stages ...shutdowner) func(context.Context) error {
	return func(ctx context.Context) error {
		<-ctx.Done()
		setupLog.Info("shutting down otel runtime")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
		defer cancel()
		for _, stage := range stages {
			if err := stage.Shutdown(shutdownCtx); err != nil {
				setupLog.Error(err, "otel shutdown", "stage", fmt.Sprintf("%T", stage))
			}
		}
		return nil
	}
}

// leaseTiming is how the manager holds its leader lease.
type leaseTiming struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// The lease timing defaults to controller-runtime's own. client-go times out
// each lease request at half the renew deadline, so with these values one
// write slower than 5 s, retried once, loses the lease. A deployment whose API
// server answers Lease writes slowly sets longer values through the flags; the
// cost is that a standby waits up to the lease duration after a leader that
// did not release the lease.
const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
)

// bindLeaseFlags registers the lease timing flags on fs.
func bindLeaseFlags(fs *flag.FlagSet) *leaseTiming {
	lease := &leaseTiming{}
	fs.DurationVar(&lease.LeaseDuration, "leader-elect-lease-duration", defaultLeaseDuration,
		"How long a standby waits before it takes over a lease that was not renewed.")
	fs.DurationVar(&lease.RenewDeadline, "leader-elect-renew-deadline", defaultRenewDeadline,
		"How long the leader keeps retrying a renewal before it gives the lease up. "+
			"Each renewal request may take half of it.")
	fs.DurationVar(&lease.RetryPeriod, "leader-elect-retry-period", defaultRetryPeriod,
		"How long the leader waits between renewal attempts.")
	return lease
}

// managerOptions returns the manager's options. The leader lease is released
// when the manager stops; runUntilManagerStops then ends the process, which
// LeaderElectionReleaseOnCancel requires.
func managerOptions(metricsAddr, probeAddr string, leaderElect bool, lease *leaseTiming) ctrl.Options {
	return ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                leaderElect,
		LeaderElectionID:              "resourcemetrics.miloapis.com",
		LeaderElectionNamespace:       "milo-system",
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &lease.LeaseDuration,
		RenewDeadline:                 &lease.RenewDeadline,
		RetryPeriod:                   &lease.RetryPeriod,
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
		Endpoint:              cfg.Endpoint,
		Insecure:              cfg.Insecure,
		Headers:               cfg.Headers,
		CollectionInterval:    cfg.CollectionInterval.Duration,
		MaxExportRequestBytes: cfg.MaxExportRequestBytes,
		ServiceName:           "resource-metrics",
		ResourceAttributes:    attrs,
	}
}

type runnableProvider interface {
	multicluster.Provider
	multicluster.ProviderRunnable
}

type managedProvider struct {
	multicluster.Provider
}

// rootClusterEngager engages the root/management cluster before delegating
// to the underlying provider's Start. Used in Milo mode when
// CollectRootControlPlane is enabled to also collect metrics from the
// cluster where project discovery runs.
type rootClusterEngager struct {
	runnableProvider
	rootCluster cluster.Cluster
}

func (r *rootClusterEngager) Start(ctx context.Context, aware multicluster.Aware) error {
	if err := aware.Engage(ctx, "root", r.rootCluster); err != nil {
		return err
	}
	return r.runnableProvider.Start(ctx, aware)
}

// Get overrides the embedded provider so that the "root" cluster name resolves
// to the root control plane instead of returning ErrClusterNotFound from the
// underlying Milo provider, which is unaware of the pre-engaged root cluster.
func (r *rootClusterEngager) Get(ctx context.Context, clusterName multicluster.ClusterName) (cluster.Cluster, error) {
	if clusterName == "root" {
		return r.rootCluster, nil
	}
	return r.runnableProvider.Get(ctx, clusterName)
}

// engageInterceptor wraps an mcmanager.Manager and forwards every Engage call
// to both the underlying manager and a ClusterManager. This is how we hook
// per-cluster collector creation without modifying upstream providers.
type engageInterceptor struct {
	multicluster.Aware
	clusterManager *collector.ClusterManager
}

// Engage is called by the provider when a control plane becomes ready. We
// engage the ClusterManager first; if that fails we do not engage the
// underlying manager so the provider sees the failure atomically.
func (i *engageInterceptor) Engage(ctx context.Context, name multicluster.ClusterName, cl cluster.Cluster) error {
	if err := i.clusterManager.Engage(ctx, string(name), cl); err != nil {
		return err
	}
	return i.Aware.Engage(ctx, name, cl)
}

// collectorProviderWrapper intercepts the Aware passed to
// provider.Start so that every Engage routed through it is observed by the
// ClusterManager.
type collectorProviderWrapper struct {
	runnableProvider
	clusterManager *collector.ClusterManager
}

func (w *collectorProviderWrapper) Start(ctx context.Context, aware multicluster.Aware) error {
	wrapped := &engageInterceptor{Aware: aware, clusterManager: w.clusterManager}
	return w.runnableProvider.Start(ctx, wrapped)
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
		if serverConfig.Discovery.CollectRootControlPlane {
			setupLog.Info("WARNING: collectRootControlPlane has no effect in single discovery mode")
		}
		deploymentCluster, err := cluster.New(ctrl.GetConfigOrDie(), func(o *cluster.Options) {
			o.Scheme = scheme
		})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to construct deployment cluster: %w", err)
		}
		runnables = append(runnables, deploymentCluster)
		provider = mcsingle.New("single", deploymentCluster)

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

		var rp runnableProvider = miloProvider
		if serverConfig.Discovery.CollectRootControlPlane {
			setupLog.Info("root control plane collection enabled; engaging root cluster as \"root\"")
			rootCluster, err := cluster.New(discoveryRestConfig, func(o *cluster.Options) {
				o.Scheme = scheme
			})
			if err != nil {
				return nil, nil, fmt.Errorf("creating root control plane cluster: %w", err)
			}
			runnables = append(runnables, rootCluster)
			rp = &rootClusterEngager{runnableProvider: miloProvider, rootCluster: rootCluster}
		}
		provider = rp
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
