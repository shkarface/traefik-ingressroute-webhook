package validator

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"

	httpmuxer "github.com/traefik/traefik/v3/pkg/muxer/http"
	traefikv1alpha1 "github.com/traefik/traefik/v3/pkg/provider/kubernetes/crd/traefikio/v1alpha1"
)

// Lookup gives the validator read access to cluster state.
type Lookup interface {
	ListIngressRoutes(ctx context.Context) ([]traefikv1alpha1.IngressRoute, error)
	MiddlewareExists(ctx context.Context, namespace, name string) (bool, error)
	ServiceExists(ctx context.Context, kind, namespace, name string) (bool, error)
}

// Result separates hard failures from advisory warnings. Missing references
// are warnings because GitOps tools often apply a route before its middleware.
type Result struct {
	Violations []string
	Warnings   []string
}

func (r *Result) deny(format string, args ...any) {
	r.Violations = append(r.Violations, fmt.Sprintf(format, args...))
}

func (r *Result) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

type Validator struct {
	Lookup Lookup
	parser httpmuxer.SyntaxParser
}

func New(lookup Lookup) (*Validator, error) {
	parser, err := httpmuxer.NewSyntaxParser()
	if err != nil {
		return nil, fmt.Errorf("creating Traefik rule parser: %w", err)
	}
	return &Validator{Lookup: lookup, parser: parser}, nil
}

func (v *Validator) Validate(ctx context.Context, ir *traefikv1alpha1.IngressRoute) (Result, error) {
	var res Result
	ns := ir.Namespace

	if len(ir.Spec.Routes) == 0 {
		res.deny("spec.routes must contain at least one route")
	}

	seenMatch := map[string]int{}
	for i, r := range ir.Spec.Routes {
		path := fmt.Sprintf("spec.routes[%d]", i)

		syntax := r.Syntax //nolint:staticcheck // Deprecated, but still honored by Traefik, so it must be validated.
		if err := v.checkRule(r.Match, syntax); err != nil {
			res.deny("%s.match: %v", path, err)
		}
		if syntax == "v2" {
			res.warn("%s.syntax: v2 rule syntax is deprecated, rewrite the rule using v3 syntax", path)
		}

		norm := normalizeMatch(r.Match)
		if j, dup := seenMatch[norm]; dup {
			res.deny("%s.match duplicates spec.routes[%d].match", path, j)
		}
		seenMatch[norm] = i

		for j, m := range r.Middlewares {
			mns := refNamespace(m.Namespace, ns)
			ok, err := v.Lookup.MiddlewareExists(ctx, mns, m.Name)
			if err != nil {
				return res, fmt.Errorf("looking up middleware %s/%s: %w", mns, m.Name, err)
			}
			if !ok {
				res.warn("%s.middlewares[%d]: Middleware %s/%s not found", path, j, mns, m.Name)
			}
		}

		for j, s := range r.Services {
			sns := refNamespace(s.Namespace, ns)
			kind := s.Kind
			if kind == "" {
				kind = "Service"
			}
			ok, err := v.Lookup.ServiceExists(ctx, kind, sns, s.Name)
			if err != nil {
				return res, fmt.Errorf("looking up %s %s/%s: %w", kind, sns, s.Name, err)
			}
			if !ok {
				res.warn("%s.services[%d]: %s %s/%s not found", path, j, kind, sns, s.Name)
			}
		}
	}

	if err := v.checkConflicts(ctx, ir, &res); err != nil {
		return res, err
	}
	return res, nil
}

// checkRule parses the rule with Traefik's own HTTP muxer, so syntax errors,
// unknown matchers and wrong arity are reported exactly as Traefik would.
func (v *Validator) checkRule(rule, syntax string) error {
	if strings.TrimSpace(rule) == "" {
		return fmt.Errorf("rule is empty")
	}
	if syntax != "" && syntax != "v2" && syntax != "v3" {
		return fmt.Errorf("unknown syntax %q, expected v2 or v3", syntax)
	}
	mux := httpmuxer.NewMuxer(v.parser, nil)
	return mux.AddRoute(rule, syntax, 0, "", nil)
}

// checkConflicts rejects rules already used by another IngressRoute on an
// overlapping entry point.
func (v *Validator) checkConflicts(ctx context.Context, ir *traefikv1alpha1.IngressRoute, res *Result) error {
	others, err := v.Lookup.ListIngressRoutes(ctx)
	if err != nil {
		return fmt.Errorf("listing IngressRoutes: %w", err)
	}

	for _, o := range others {
		if o.Namespace == ir.Namespace && o.Name == ir.Name {
			continue
		}
		if !entryPointsOverlap(ir.Spec.EntryPoints, o.Spec.EntryPoints) {
			continue
		}
		ref := o.Namespace + "/" + o.Name
		for i, r := range ir.Spec.Routes {
			norm := normalizeMatch(r.Match)
			for _, or := range o.Spec.Routes {
				if normalizeMatch(or.Match) == norm {
					res.deny("spec.routes[%d].match %q is already used by IngressRoute %s on an overlapping entry point", i, r.Match, ref)
				}
			}
		}
	}
	return nil
}

// normalizeMatch collapses whitespace and quote style so that identical rules
// written differently compare equal.
func normalizeMatch(rule string) string {
	var b strings.Builder
	inStr := rune(0)
	for _, c := range rule {
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
				b.WriteRune('`')
				continue
			}
			b.WriteRune(c)
		case c == '`' || c == '"':
			inStr = c
			b.WriteRune('`')
		case unicode.IsSpace(c):
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// entryPointsOverlap treats an empty list as "all entry points", like Traefik.
func entryPointsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, ep := range a {
		if slices.Contains(b, ep) {
			return true
		}
	}
	return false
}

func refNamespace(ref, fallback string) string {
	if ref == "" {
		return fallback
	}
	return ref
}
