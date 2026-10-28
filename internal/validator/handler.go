package validator

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	traefikv1alpha1 "github.com/traefik/traefik/v3/pkg/provider/kubernetes/crd/traefikio/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Handler adapts the Validator to a controller-runtime admission webhook.
type Handler struct {
	Validator *Validator
}

var _ admission.Handler = &Handler{}

func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("ingressroute", req.Namespace+"/"+req.Name, "operation", req.Operation)

	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}

	var ir traefikv1alpha1.IngressRoute
	if err := json.Unmarshal(req.Object.Raw, &ir); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if ir.Namespace == "" {
		ir.Namespace = req.Namespace
	}
	if ir.Name == "" {
		// generateName: no stored object can share this name.
		ir.Name = req.Name
	}

	res, err := h.Validator.Validate(ctx, &ir)
	if err != nil {
		log.Error(err, "validation failed")
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if len(res.Violations) > 0 {
		log.Info("denied", "violations", res.Violations)
		return admission.Denied(strings.Join(res.Violations, "; ")).WithWarnings(res.Warnings...)
	}
	return admission.Allowed("").WithWarnings(res.Warnings...)
}

// KubeLookup implements Lookup against the API server. Cached reads are used
// for Traefik CRDs; Services go through the uncached reader so the webhook
// does not have to watch every Service in the cluster.
type KubeLookup struct {
	Cached   client.Reader
	Uncached client.Reader
}

func (k *KubeLookup) ListIngressRoutes(ctx context.Context) ([]traefikv1alpha1.IngressRoute, error) {
	var list traefikv1alpha1.IngressRouteList
	if err := k.Cached.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k *KubeLookup) MiddlewareExists(ctx context.Context, namespace, name string) (bool, error) {
	return exists(ctx, k.Cached, namespace, name, &traefikv1alpha1.Middleware{})
}

func (k *KubeLookup) ServiceExists(ctx context.Context, kind, namespace, name string) (bool, error) {
	if kind == "TraefikService" {
		return exists(ctx, k.Cached, namespace, name, &traefikv1alpha1.TraefikService{})
	}
	return exists(ctx, k.Uncached, namespace, name, &corev1.Service{})
}

func exists(ctx context.Context, r client.Reader, namespace, name string, obj client.Object) (bool, error) {
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
