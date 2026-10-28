# traefik-ingressroute-webhook

Validating admission webhook for Traefik `IngressRoute` (`traefik.io/v1alpha1`).
It uses Traefik's own CRD types and HTTP rule parser (`github.com/traefik/traefik/v3`),
so rule validation matches what Traefik itself accepts.

## Rules

| Rule | Result |
| --- | --- |
| `match` must parse with Traefik's v3 (or `syntax: v2`) parser | deny |
| Same normalized `match` used by another IngressRoute on an overlapping entry point | deny |
| Duplicate `match` inside one IngressRoute | deny |
| Referenced Middleware / Service / TraefikService not found | warning |
| `syntax: v2` used | warning |

Missing references are warnings, not denials, so GitOps tools can apply a route
before its middleware.

## Install

The chart is published as an OCI artifact. cert-manager issues the serving
certificate by default; without cert-manager, create the certificate yourself as
described in the [chart README](charts/traefik-ingressroute-webhook/README.md#without-cert-manager).

```bash
helm install traefik-ingressroute-webhook \
  oci://ghcr.io/shkarface/charts/traefik-ingressroute-webhook \
  --namespace traefik-ingressroute-webhook --create-namespace
```

See the [chart README](charts/traefik-ingressroute-webhook/README.md) for options.

## Development

```bash
go test -race ./...
golangci-lint run
helm lint charts/traefik-ingressroute-webhook
docker build -t traefik-ingressroute-webhook:dev .
```

## Releasing

Push a `vX.Y.Z` tag. The release workflow builds and pushes a multi-arch image
to `ghcr.io/shkarface/traefik-ingressroute-webhook`, pushes the chart (with
`version` and `appVersion` set to `X.Y.Z`) to
`oci://ghcr.io/shkarface/charts`, and creates a GitHub release.

## Bumping Traefik

Traefik's `go.mod` has `replace` directives that Go does not apply to consumers.
They are mirrored in this repo's `go.mod`, and the nested `dynamic/ext` module is
copied into `third_party/traefik-dynamic-ext`. Re-check both when upgrading.

## Limits

- Conflict checks read from an informer cache, so two conflicting routes created
  within the same instant can both be admitted.
- `traefik.containo.us` (legacy v2 API group), `IngressRouteTCP` and
  `IngressRouteUDP` are not validated.
