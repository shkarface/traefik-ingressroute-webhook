# traefik-ingressroute-webhook chart

Validating admission webhook for Traefik `IngressRoute` (`traefik.io/v1alpha1`).

```bash
helm install traefik-ingressroute-webhook \
  oci://ghcr.io/shkarface/charts/traefik-ingressroute-webhook \
  --namespace traefik-ingressroute-webhook --create-namespace
```

## Serving certificate

The API server only calls webhooks over HTTPS, and trusts them through the
`caBundle` in the ValidatingWebhookConfiguration. The chart supports two ways to
provide the certificate.

### With cert-manager (default)

The chart creates a `Certificate`; cert-manager writes the Secret, renews it, and
injects its CA into the webhook configuration. By default a self-signed `Issuer`
is created in the release namespace. To use an existing issuer:

```yaml
certManager:
  issuerRef:
    kind: ClusterIssuer
    name: internal-ca
```

### Without cert-manager

Create the certificate and Secret yourself before installing, then point the
chart at them:

```yaml
certManager:
  enabled: false
existingSecret:
  name: traefik-ingressroute-webhook-tls
  caBundle: <base64 of ca.crt>
```

The commands below work with OpenSSL 3+, and with LibreSSL as shipped on macOS.

1. Set the namespace and the Service name. The Service is named after the
   release: the release name if it contains `traefik-ingressroute-webhook`,
   otherwise `<release>-traefik-ingressroute-webhook`, or `fullnameOverride`
   if set.

   ```bash
   NS=traefik-ingressroute-webhook
   SVC=traefik-ingressroute-webhook
   ```

2. Create a CA. Its key only signs the serving certificate, so keep it offline
   (or delete it and create a new CA when you rotate).

   ```bash
   openssl genrsa -out ca.key 2048
   openssl req -new -key ca.key -subj "/CN=$SVC-ca" -out ca.csr
   printf 'basicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n' > ca.ext
   openssl x509 -req -in ca.csr -signkey ca.key -days 3650 -extfile ca.ext -out ca.crt
   ```

3. Create the serving certificate for the Service DNS names.

   ```bash
   openssl genrsa -out tls.key 2048
   openssl req -new -key tls.key -subj "/CN=$SVC.$NS.svc" -out tls.csr
   printf 'subjectAltName=DNS:%s.%s.svc,DNS:%s.%s.svc.cluster.local\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature,keyEncipherment\n' \
     "$SVC" "$NS" "$SVC" "$NS" > tls.ext
   openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 365 -extfile tls.ext -out tls.crt
   openssl verify -CAfile ca.crt tls.crt
   ```

4. Create the Secret and install the chart.

   ```bash
   kubectl create namespace "$NS"
   kubectl -n "$NS" create secret tls "$SVC-tls" --cert=tls.crt --key=tls.key
   helm install "$SVC" oci://ghcr.io/shkarface/charts/traefik-ingressroute-webhook \
     --namespace "$NS" \
     --set certManager.enabled=false \
     --set existingSecret.name="$SVC-tls" \
     --set existingSecret.caBundle="$(base64 < ca.crt | tr -d '\n')"
   ```

5. Rotate before `tls.crt` expires: repeat step 3 with the same CA and replace
   the Secret. The webhook reloads the certificate without a restart, and the
   `caBundle` stays the same. Only a new CA requires updating
   `existingSecret.caBundle`.

   ```bash
   kubectl -n "$NS" create secret tls "$SVC-tls" --cert=tls.crt --key=tls.key \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

Keep `tls.key` and `ca.key` out of git. With GitOps, store the Secret through
your usual secret tooling (for example SOPS or External Secrets); `caBundle` is
public and can live in values.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `replicaCount` | `2` | Keep at least 2: with `failurePolicy: Fail`, IngressRoute writes are blocked while no replica is ready. |
| `image.repository` | `ghcr.io/shkarface/traefik-ingressroute-webhook` | |
| `image.tag` | chart `appVersion` | |
| `logLevel` | `info` | `debug`, `info`, `error`, or an integer verbosity. |
| `webhook.failurePolicy` | `Fail` | `Ignore` admits IngressRoutes unvalidated while the webhook is down. |
| `webhook.timeoutSeconds` | `5` | |
| `webhook.excludedNamespaces` | `[kube-system]` | The release namespace is always excluded. |
| `webhook.namespaceSelector` | `{}` | Replaces the selector built from `excludedNamespaces`. |
| `webhook.objectSelector` | `{}` | |
| `certManager.enabled` | `true` | |
| `certManager.issuerRef` | `{}` | Empty creates a self-signed `Issuer`. |
| `certManager.duration` / `renewBefore` | `8760h` / `720h` | |
| `existingSecret.name` | `""` | Required when `certManager.enabled` is false. |
| `existingSecret.caBundle` | `""` | Base64 PEM CA. Required when `certManager.enabled` is false. |
| `metrics.enabled` | `true` | Prometheus metrics on `metrics.port` (8080). |
| `metrics.serviceMonitor.enabled` | `false` | Requires the Prometheus Operator CRDs. |
| `podDisruptionBudget.enabled` | `true` | `minAvailable: 1`. |
| `topologySpreadConstraints` | `[]` | Empty spreads replicas across nodes. |

See [values.yaml](values.yaml) for the rest: pod and container security
contexts, resources, scheduling, service account and labels.
