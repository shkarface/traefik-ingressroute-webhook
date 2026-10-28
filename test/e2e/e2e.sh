#!/usr/bin/env bash
# End-to-end test against a throwaway Kind cluster.
#
# Usage: test/e2e/e2e.sh [cert-manager|existing-secret|all]
#
# Environment:
#   KIND          kind binary (default: kind)
#   CLUSTER       cluster name (default: tirw-e2e)
#   KEEP_CLUSTER  set to 1 to keep the cluster and print its kubeconfig
#
# The cluster uses its own kubeconfig, so the current kubectl context is not
# touched.
#
# shellcheck disable=SC2016 # Backticks in single quotes are Traefik rule literals.
set -euo pipefail

MODE=${1:-all}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
KIND=${KIND:-kind}
CLUSTER=${CLUSTER:-tirw-e2e}
IMAGE=ghcr.io/shkarface/traefik-ingressroute-webhook:e2e
NS=traefik-ingressroute-webhook
SVC=traefik-ingressroute-webhook
CHART=$ROOT/charts/traefik-ingressroute-webhook
CERT_MANAGER_VERSION=v1.21.3
TRAEFIK_CRDS_VERSION=1.18.0
OPENSSL_IMAGE=alpine/openssl:3.5.9

case $MODE in
  cert-manager | existing-secret | all) ;;
  *) echo "usage: $0 [cert-manager|existing-secret|all]" >&2; exit 2 ;;
esac

WORK=$(mktemp -d)
export KUBECONFIG=$WORK/kubeconfig
passed=0
failed=0

log() { printf '\n==> %s\n' "$*"; }

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 || $failed -ne 0 ]]; then
    log "webhook logs"
    kubectl -n "$NS" logs -l app.kubernetes.io/name=traefik-ingressroute-webhook --tail=50 --prefix 2>/dev/null || true
  fi
  if [[ ${KEEP_CLUSTER:-0} == 1 ]]; then
    log "keeping cluster $CLUSTER, export KUBECONFIG=$KUBECONFIG"
  else
    "$KIND" delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true
    rm -rf -- "$WORK"
  fi
}
trap cleanup EXIT

# --- assertions ---------------------------------------------------------------

# ir NAMESPACE NAME MATCH [EXTRA_ROUTE_YAML] [ENTRYPOINTS]
ir() {
  cat <<EOF
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata: {name: $2, namespace: $1}
spec:
  entryPoints: ${5:-[]}
  routes:
    - match: '$3'
      services: [{name: app, port: 80}]
$4
EOF
}

# expect LABEL allow|deny|warn EXPECTED_SUBSTRING YAML
expect() {
  local label=$1 want=$2 needle=$3 yaml=$4 out rc got
  if out=$(printf '%s\n' "$yaml" | kubectl apply -f - 2>&1); then rc=0; else rc=$?; fi
  if [[ $rc -ne 0 ]]; then
    got=deny
    [[ $out == *"denied the request"* ]] || got=error
  elif [[ $out == *"Warning:"* ]]; then
    got=warn
  else
    got=allow
  fi
  if [[ $got == "$want" && $out == *"$needle"* ]]; then
    passed=$((passed + 1))
    printf 'PASS %s\n' "$label"
  else
    failed=$((failed + 1))
    printf 'FAIL %s: want %s containing %q, got %s:\n%s\n' "$label" "$want" "$needle" "$got" "$out"
  fi
}

check() { # LABEL COMMAND...
  local label=$1
  shift
  if "$@"; then
    passed=$((passed + 1))
    printf 'PASS %s\n' "$label"
  else
    failed=$((failed + 1))
    printf 'FAIL %s\n' "$label"
  fi
}

# --- cluster helpers ----------------------------------------------------------

webhook_pods() {
  kubectl -n "$NS" get pods -l app.kubernetes.io/name=traefik-ingressroute-webhook \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

# Pods can be Ready a moment before kube-proxy routes to them.
wait_admission() {
  for _ in $(seq 1 30); do
    if ir team-a probe 'Host(`probe.example.com`)' '' | kubectl apply --dry-run=server -f - >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "webhook did not start admitting requests" >&2
  return 1
}

reset_fixtures() {
  kubectl create ns team-a --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl create ns team-b --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  for ns in team-a team-b; do
    kubectl -n "$ns" delete ingressroutes.traefik.io --all >/dev/null
    kubectl -n "$ns" create service clusterip app --tcp=80:80 --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  done
  kubectl apply -f - >/dev/null <<'EOF'
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata: {name: auth, namespace: team-a}
spec:
  headers:
    customRequestHeaders: {X-Test: "1"}
EOF
}

# served_cert prints the TLS handshake with the webhook Service, offering both
# h2 and http/1.1 via ALPN. The pod runs to completion before its logs are read;
# attaching with `kubectl run -i` races a short-lived s_client.
served_cert() {
  local pod phase
  pod="tlscheck-$RANDOM"
  kubectl -n "$NS" run "$pod" --restart=Never --image="$OPENSSL_IMAGE" -- \
    s_client -connect "$SVC.$NS.svc:443" -servername "$SVC.$NS.svc" -alpn h2,http/1.1 >/dev/null
  for _ in $(seq 1 60); do
    phase=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.phase}')
    [[ $phase == Succeeded || $phase == Failed ]] && break
    sleep 2
  done
  kubectl -n "$NS" logs "$pod"
  kubectl -n "$NS" delete pod "$pod" --wait=false >/dev/null
}

negotiates_http11_only() { served_cert | grep -q 'ALPN protocol: http/1.1'; }

serves_serial() { served_cert | openssl x509 -noout -serial | grep -qx "$1"; }

# --- test suites --------------------------------------------------------------

run_cases() {
  reset_fixtures
  wait_admission

  expect "valid route" allow "created" \
    "$(ir team-a web 'Host(`a.example.com`)' '      middlewares: [{name: auth}]')"
  expect "update of the same route is not a conflict" allow "configured" \
    "$(ir team-a web 'Host(`a.example.com`)' '      priority: 10')"
  expect "unterminated string" deny "spec.routes[0].match" \
    "$(ir team-a bad1 'Host(`a.example.com)' '')"
  expect "invalid regex" deny "error parsing regexp" \
    "$(ir team-a bad2 'PathRegexp(`[`)' '')"
  expect "invalid ClientIP" deny "ClientIP" \
    "$(ir team-a bad3 'ClientIP(`not-an-ip`)' '')"
  expect "v2 multi-host without syntax: v2" deny "spec.routes[0].match" \
    "$(ir team-a bad4 'Host(`a.com`, `b.com`)' '')"
  expect "unknown matcher" deny "spec.routes[0].match" \
    "$(ir team-a bad5 'Headers(`X`, `y`)' '')"
  expect "duplicate match in another namespace" deny "already used by IngressRoute team-a/web" \
    "$(ir team-b dup 'Host("a.example.com")' '')"
  expect "duplicate match inside one IngressRoute" deny "spec.routes[1].match duplicates spec.routes[0].match" \
    "$(ir team-a dup2 'Host(`c.example.com`)' "    - match: 'Host(\`c.example.com\`)'
      services: [{name: app, port: 80}]")"
  expect "missing middleware warns" warn "Middleware team-a/nope not found" \
    "$(ir team-a mw 'Host(`b.example.com`)' '      middlewares: [{name: nope}]')"
  expect "missing service warns" warn "Service team-a/ghost not found" \
    "$(ir team-a svc 'Host(`d.example.com`)' "    - match: 'Host(\`e.example.com\`)'
      services: [{name: ghost, port: 80}]")"
  expect "syntax v2 warns" warn "v2 rule syntax is deprecated" \
    "$(ir team-a v2 'Host(`f.example.com`, `g.example.com`)' '      syntax: v2')"
  expect "same match on disjoint entry points (1/2)" allow "created" \
    "$(ir team-a ep1 'Host(`h.example.com`)' '' '[web]')"
  expect "same match on disjoint entry points (2/2)" allow "created" \
    "$(ir team-b ep2 'Host(`h.example.com`)' '' '[websecure]')"
  expect "same match on an overlapping entry point" deny "already used by IngressRoute team-a/ep1" \
    "$(ir team-b ep3 'Host(`h.example.com`)' '' '[web, websecure]')"
  expect "release namespace is not validated" allow "created" \
    "$(ir "$NS" inns 'Host(`a.example.com`)' '')"
  kubectl -n "$NS" delete ingressroute inns >/dev/null

  check "HTTP/2 is not negotiated" negotiates_http11_only
}

# Mirrors the "Without cert-manager" steps in the chart README; keep in sync.
gen_ca() {
  openssl genrsa -out ca.key 2048
  openssl req -new -key ca.key -subj "/CN=$SVC-ca" -out ca.csr
  printf 'basicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n' >ca.ext
  openssl x509 -req -in ca.csr -signkey ca.key -days 3650 -extfile ca.ext -out ca.crt
}

gen_serving_cert() {
  openssl genrsa -out tls.key 2048
  openssl req -new -key tls.key -subj "/CN=$SVC.$NS.svc" -out tls.csr
  printf 'subjectAltName=DNS:%s.%s.svc,DNS:%s.%s.svc.cluster.local\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature,keyEncipherment\n' \
    "$SVC" "$NS" "$SVC" "$NS" >tls.ext
  openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 365 -extfile tls.ext -out tls.crt
  openssl verify -CAfile ca.crt tls.crt
}

suite_cert_manager() {
  log "install chart with cert-manager"
  helm upgrade --install "$SVC" "$CHART" -n "$NS" --create-namespace \
    --set image.tag=e2e --set image.pullPolicy=Never --wait --timeout 3m
  run_cases
  helm uninstall "$SVC" -n "$NS" --wait
}

suite_existing_secret() {
  log "install chart with an existing Secret"
  mkdir -p "$WORK/certs"
  (
    cd "$WORK/certs"
    gen_ca >/dev/null 2>&1
    gen_serving_cert
  )
  kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  # cert-manager keeps its Secret when the Certificate is deleted.
  kubectl -n "$NS" delete secret "$SVC-tls" --ignore-not-found >/dev/null
  kubectl -n "$NS" create secret tls "$SVC-tls" --cert="$WORK/certs/tls.crt" --key="$WORK/certs/tls.key"
  helm upgrade --install "$SVC" "$CHART" -n "$NS" \
    --set certManager.enabled=false \
    --set existingSecret.name="$SVC-tls" \
    --set existingSecret.caBundle="$(base64 <"$WORK/certs/ca.crt" | tr -d '\n')" \
    --set image.tag=e2e --set image.pullPolicy=Never --wait --timeout 3m
  run_cases

  log "rotate the serving certificate"
  (cd "$WORK/certs" && gen_serving_cert >/dev/null 2>&1)
  local want
  want=$(openssl x509 -in "$WORK/certs/tls.crt" -noout -serial)
  kubectl -n "$NS" create secret tls "$SVC-tls" --cert="$WORK/certs/tls.crt" --key="$WORK/certs/tls.key" \
    --dry-run=client -o yaml | kubectl replace -f -

  # Each pod logs one reload at startup and one after the kubelet syncs the
  # updated Secret (up to about a minute).
  local reloaded=0 pod
  for _ in $(seq 1 36); do
    reloaded=1
    while read -r pod; do
      [[ $(kubectl -n "$NS" logs "$pod" | grep -c 'Updated current TLS certificate') -ge 2 ]] || reloaded=0
    done < <(webhook_pods)
    [[ $reloaded == 1 ]] && break
    sleep 5
  done
  check "all pods reloaded the rotated certificate" test "$reloaded" = 1
  check "no pod restarted" test "$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=traefik-ingressroute-webhook \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{end}' | tr -d 0)" = ""
  check "the rotated certificate is served" serves_serial "$want"
  expect "admission works after rotation" allow "created" "$(ir team-a rot 'Host(`r.example.com`)' '')"

  helm uninstall "$SVC" -n "$NS" --wait
}

# --- main ---------------------------------------------------------------------

log "create Kind cluster $CLUSTER"
"$KIND" create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 120s

log "build and load image"
docker build -q --build-arg VERSION=0.0.0-e2e -t "$IMAGE" "$ROOT"
"$KIND" load docker-image "$IMAGE" --name "$CLUSTER"

log "install cert-manager $CERT_MANAGER_VERSION and Traefik CRDs $TRAEFIK_CRDS_VERSION"
helm install cert-manager oci://quay.io/jetstack/charts/cert-manager --version "$CERT_MANAGER_VERSION" \
  -n cert-manager --create-namespace --set crds.enabled=true --wait --timeout 5m
helm install traefik-crds traefik-crds --repo https://traefik.github.io/charts --version "$TRAEFIK_CRDS_VERSION" \
  -n traefik --create-namespace --wait

if [[ $MODE == cert-manager || $MODE == all ]]; then
  suite_cert_manager
fi
if [[ $MODE == existing-secret || $MODE == all ]]; then
  suite_existing_secret
fi

log "passed=$passed failed=$failed"
[[ $failed -eq 0 ]]
