package validator

import (
	"context"
	"strings"
	"testing"

	traefikv1alpha1 "github.com/traefik/traefik/v3/pkg/provider/kubernetes/crd/traefikio/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeLookup struct {
	routes      []traefikv1alpha1.IngressRoute
	middlewares map[string]bool // "ns/name"
	services    map[string]bool // "Kind/ns/name"
}

func (f *fakeLookup) ListIngressRoutes(context.Context) ([]traefikv1alpha1.IngressRoute, error) {
	return f.routes, nil
}

func (f *fakeLookup) MiddlewareExists(_ context.Context, ns, name string) (bool, error) {
	return f.middlewares[ns+"/"+name], nil
}

func (f *fakeLookup) ServiceExists(_ context.Context, kind, ns, name string) (bool, error) {
	return f.services[kind+"/"+ns+"/"+name], nil
}

func route(ns, name string, eps []string, matches ...string) traefikv1alpha1.IngressRoute {
	ir := traefikv1alpha1.IngressRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       traefikv1alpha1.IngressRouteSpec{EntryPoints: eps},
	}
	for _, m := range matches {
		r := traefikv1alpha1.Route{Match: m}
		r.Services = []traefikv1alpha1.Service{{LoadBalancerSpec: traefikv1alpha1.LoadBalancerSpec{Name: "app"}}}
		ir.Spec.Routes = append(ir.Spec.Routes, r)
	}
	return ir
}

func newValidator(t *testing.T, l *fakeLookup) *Validator {
	t.Helper()
	if l.services == nil {
		l.services = map[string]bool{}
	}
	v, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func validate(t *testing.T, v *Validator, ir traefikv1alpha1.IngressRoute) Result {
	t.Helper()
	res, err := v.Validate(context.Background(), &ir)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func assertContains(t *testing.T, kind string, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if strings.Contains(g, want) {
			return
		}
	}
	t.Errorf("expected %s containing %q, got %q", kind, want, got)
}

func TestRuleSyntax(t *testing.T) {
	tests := []struct {
		name   string
		match  string
		syntax string
		valid  bool
	}{
		{"simple host", "Host(`example.com`)", "", true},
		{"host and path", "Host(`example.com`) && PathPrefix(`/api`)", "", true},
		{"grouped or", "(Host(`a.com`) || Host(`b.com`)) && !Path(`/x`)", "", true},
		{"header two args", "Header(`X-Env`, `prod`)", "", true},
		{"empty", "  ", "", false},
		{"unbalanced", "Host(`example.com`", "", false},
		{"unterminated string", "Host(`example.com)", "", false},
		{"unknown matcher", "Headers(`X`, `y`)", "", false},
		{"v2 multi host in v3", "Host(`a.com`, `b.com`)", "", false},
		{"v2 multi host with v2 syntax", "Host(`a.com`, `b.com`)", "v2", true},
		{"bad syntax value", "Host(`a.com`)", "v4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newValidator(t, &fakeLookup{})
			ir := route("team-a", "web", nil, tt.match)
			ir.Spec.Routes[0].Syntax = tt.syntax //nolint:staticcheck // Exercising the deprecated field.
			ir.Spec.Routes[0].Services = nil
			res := validate(t, v, ir)
			if got := len(res.Violations) == 0; got != tt.valid {
				t.Errorf("valid=%v, want %v, violations=%q", got, tt.valid, res.Violations)
			}
		})
	}
}

func TestMissingReferencesWarn(t *testing.T) {
	l := &fakeLookup{
		middlewares: map[string]bool{"team-a/present": true},
		services:    map[string]bool{"Service/team-a/app": true},
	}
	v := newValidator(t, l)
	ir := route("team-a", "web", nil, "Host(`a.com`)")
	ir.Spec.Routes[0].Middlewares = []traefikv1alpha1.MiddlewareRef{{Name: "present"}, {Name: "missing"}}
	ir.Spec.Routes[0].Services = append(ir.Spec.Routes[0].Services, traefikv1alpha1.Service{
		LoadBalancerSpec: traefikv1alpha1.LoadBalancerSpec{Name: "wrr", Kind: "TraefikService"},
	})

	res := validate(t, v, ir)
	if len(res.Violations) != 0 {
		t.Errorf("missing references must not deny, got %q", res.Violations)
	}
	if len(res.Warnings) != 2 {
		t.Errorf("expected 2 warnings, got %q", res.Warnings)
	}
	assertContains(t, "warning", res.Warnings, "Middleware team-a/missing not found")
	assertContains(t, "warning", res.Warnings, "TraefikService team-a/wrr not found")
}

func TestDuplicateMatch(t *testing.T) {
	l := &fakeLookup{routes: []traefikv1alpha1.IngressRoute{
		route("team-a", "existing", []string{"websecure"}, "Host(`a.com`) && PathPrefix(`/api`)"),
	}}
	v := newValidator(t, l)

	// Whitespace and quote style differences are still duplicates.
	res := validate(t, v, route("team-a", "new", []string{"websecure"}, `Host("a.com")&&PathPrefix(  "/api")`))
	assertContains(t, "violation", res.Violations, "already used by IngressRoute team-a/existing")

	// Updating the existing object itself is not a conflict.
	res = validate(t, v, route("team-a", "existing", []string{"websecure"}, "Host(`a.com`) && PathPrefix(`/api`)"))
	if len(res.Violations) != 0 {
		t.Errorf("unexpected violations %q", res.Violations)
	}

	// Disjoint entry points do not conflict.
	res = validate(t, v, route("team-a", "new", []string{"web"}, "Host(`a.com`) && PathPrefix(`/api`)"))
	if len(res.Violations) != 0 {
		t.Errorf("unexpected violations %q", res.Violations)
	}

	// No entry points means all, so it overlaps.
	res = validate(t, v, route("team-a", "new", nil, "Host(`a.com`) && PathPrefix(`/api`)"))
	assertContains(t, "violation", res.Violations, "already used by IngressRoute team-a/existing")

	// Duplicates within one IngressRoute.
	res = validate(t, v, route("team-a", "new", []string{"web"}, "Host(`b.com`)", "Host(`b.com`)"))
	assertContains(t, "violation", res.Violations, "spec.routes[1].match duplicates spec.routes[0].match")
}
