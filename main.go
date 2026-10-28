package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/shkarface/traefik-ingressroute-webhook/internal/validator"
	traefikv1alpha1 "github.com/traefik/traefik/v3/pkg/provider/kubernetes/crd/traefikio/v1alpha1"
)

const webhookPath = "/validate-traefik-io-v1alpha1-ingressroute"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		port        int
		certDir     string
		metricsAddr string
		probeAddr   string
	)
	flag.IntVar(&port, "port", 9443, "Webhook server port.")
	flag.StringVar(&certDir, "cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Directory containing tls.crt and tls.key.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint address, \"0\" disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe endpoint address.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "building scheme")
		os.Exit(1)
	}
	if err := traefikv1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "building scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    port,
			CertDir: certDir,
			// HTTP/2 is disabled: the API server calls webhooks over
			// HTTP/1.1 just as well, and Go's HTTP/2 server has a recurring
			// history of denial-of-service CVEs (e.g. Rapid Reset
			// GO-2023-2102, CONTINUATION flood GO-2024-2687, and five more
			// fixed in Go 1.27.2). Serving HTTP/1.1 only removes that surface.
			TLSOpts: []func(*tls.Config){func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }},
		}),
	})
	if err != nil {
		log.Error(err, "creating manager")
		os.Exit(1)
	}

	// Start informers for the Traefik CRDs up front so the first admission
	// request does not race an empty cache.
	for _, obj := range []client.Object{&traefikv1alpha1.IngressRoute{}, &traefikv1alpha1.Middleware{}, &traefikv1alpha1.TraefikService{}} {
		if _, err := mgr.GetCache().GetInformer(context.Background(), obj); err != nil {
			log.Error(err, "creating informer", "type", fmt.Sprintf("%T", obj))
			os.Exit(1)
		}
	}

	v, err := validator.New(&validator.KubeLookup{Cached: mgr.GetClient(), Uncached: mgr.GetAPIReader()})
	if err != nil {
		log.Error(err, "creating validator")
		os.Exit(1)
	}
	mgr.GetWebhookServer().Register(webhookPath, &webhook.Admission{Handler: &validator.Handler{Validator: v}})

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "adding healthz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		log.Error(err, "adding webhook readyz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("cache", cacheSynced(mgr)); err != nil {
		log.Error(err, "adding cache readyz check")
		os.Exit(1)
	}

	log.Info("starting webhook", "version", version, "port", port)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running manager")
		os.Exit(1)
	}
}

func cacheSynced(mgr ctrl.Manager) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), time.Second)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return fmt.Errorf("informer cache not synced")
		}
		return nil
	}
}
