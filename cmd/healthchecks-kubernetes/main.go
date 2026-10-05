package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/Placetel/healthchecks-kubernetes/internal/controller"
	"github.com/Placetel/healthchecks-kubernetes/internal/healthchecks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("controller stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var config controller.Config
	var apiURL, kubeconfig, healthAddress string
	var timeout time.Duration
	var printVersion bool
	flag.StringVar(&config.Cluster, "cluster", "", "stable, unique cluster name (required)")
	flag.StringVar(&config.Namespace, "namespace", "", "watch one namespace; empty watches all")
	flag.StringVar(&config.StateNamespace, "state-namespace", env("POD_NAMESPACE", "default"), "namespace for controller state and leader Lease")
	flag.StringVar(&config.StateName, "state-name", "healthchecks-kubernetes", "name of controller state ConfigMap and leader Lease")
	flag.BoolVar(&config.DefaultInclude, "default-include", true, "monitor all CronJobs unless excluded")
	flag.IntVar(&config.Grace, "grace-seconds", 300, "default grace period, 60-31536000 seconds")
	flag.StringVar(&config.Timezone, "timezone", "UTC", "fallback timezone; match kube-controller-manager when spec.timeZone is absent")
	flag.StringVar(&config.Channels, "channels", "*", "notification integration IDs/names, * for all, empty for none")
	flag.DurationVar(&config.Resync, "resync", 5*time.Minute, "periodic reconciliation interval")
	flag.StringVar(&apiURL, "api-url", env("HEALTHCHECKS_API_URL", "https://healthchecks.io/api/v3"), "Healthchecks Management v3 API base URL")
	flag.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "optional kubeconfig file; otherwise use in-cluster credentials")
	flag.StringVar(&healthAddress, "health-address", ":8080", "health probe listen address")
	flag.DurationVar(&timeout, "http-timeout", 10*time.Second, "Healthchecks HTTP timeout")
	flag.BoolVar(&printVersion, "version", false, "print version and exit")
	flag.Parse()
	if printVersion {
		fmt.Println(version)
		return nil
	}
	if err := config.Validate(); err != nil {
		return err
	}
	backend, err := healthchecks.New(apiURL, os.Getenv("HEALTHCHECKS_API_KEY"), timeout, 650*time.Millisecond)
	if err != nil {
		return err
	}
	var kubeConfig *rest.Config
	if kubeconfig != "" {
		kubeConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		kubeConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return errors.New("cannot load Kubernetes credentials; supply --kubeconfig outside the cluster")
	}
	kubeConfig.UserAgent = "healthchecks-kubernetes/" + version
	kubeConfig.Timeout = 30 * time.Second
	kube, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return errors.New("cannot create Kubernetes client")
	}
	ctrl, err := controller.New(config, kube, backend)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ctrl.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Addr: healthAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- errors.New("health probe server failed")
			stop()
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	hostname, err := os.Hostname()
	if err != nil {
		return errors.New("cannot determine leader identity")
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: config.StateName, Namespace: config.StateNamespace},
		Client:    kube.CoordinationV1(), LockConfig: resourcelock.ResourceLockConfig{Identity: hostname + "-" + strconv.Itoa(os.Getpid())},
	}
	controllerErr := make(chan error, 1)
	controllerDone := make(chan struct{})
	// The leader holds its lease until expiry on shutdown. This avoids handing
	// leadership over while the final in-flight ping or checkpoint is completing.
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				defer close(controllerDone)
				if err := ctrl.Run(leaderCtx); err != nil {
					controllerErr <- err
					stop()
				}
			},
			OnStoppedLeading: func() { stop() },
		},
	})
	if err != nil {
		return errors.New("cannot configure leader election")
	}
	slog.Info("starting controller", "version", version, "cluster", config.Cluster, "namespace", config.Namespace)
	elector.Run(ctx)
	// OnStartedLeading runs in its own goroutine. Wait for cancellation to drain
	// work, but also handle shutdown before this process acquired leadership.
	select {
	case <-controllerDone:
	case <-time.After(timeout + time.Second):
	}
	select {
	case err := <-controllerErr:
		return err
	default:
	}
	select {
	case err := <-serverErr:
		return err
	default:
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
