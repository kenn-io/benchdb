#!/bin/bash

export BENCHDB_SERVER_IMAGE_NAME="${BENCHDB_SERVER_IMAGE_NAME:-benchdb-server}"
if [[ -z "${BENCHDB_DEPLOY_VERSION:-}" ]]; then
  echo "BENCHDB_DEPLOY_VERSION must be set to the immutable image tag for this deployment" >&2
  if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
    return 1
  fi
  exit 1
fi
export BENCHDB_SERVER_IMAGE_SPEC="${DOCKER_REGISTRY}/${BENCHDB_SERVER_IMAGE_NAME}:${BENCHDB_DEPLOY_VERSION}"
export BENCHDB_ADDR="${BENCHDB_ADDR:-:8080}"

# This script assumes that secrets have been injected via environment before
# executing this logic. That is, secrets are available here as values of a
# specific set of well-known environment variables. Not all of these
# configuration parameters are secrets. Non-sensitive parameter names include:
#
# BENCHDB_INTENDED_BASE_URL (scheme and DNS name)
# EKS_CLUSTER (the name of the EKS cluster to operate on)
# NAMESPACE (indicating the k8s namespace to deploy into)
# ...

urlencode() {
  python3 -c 'import sys; from urllib.parse import quote; print(quote(sys.stdin.read(), safe=""))'
}

build_benchdb_db_url() {
  local restore_xtrace=0
  case "$-" in
    *x*)
      restore_xtrace=1
      set +x
      ;;
  esac

  local user password host port db
  for key in DB_USERNAME DB_PASSWORD DB_HOST DB_PORT DB_NAME; do
    if [[ -z "${!key:-}" ]]; then
      echo "$key must be set" >&2
      if ((restore_xtrace)); then
        set -x
      fi
      return 1
    fi
  done
  user="$(printf '%s' "${DB_USERNAME}" | urlencode)"
  password="$(printf '%s' "${DB_PASSWORD}" | urlencode)"
  host="${DB_HOST}"
  port="${DB_PORT}"
  db="$(printf '%s' "${DB_NAME}" | urlencode)"
  printf 'postgres://%s:%s@%s:%s/%s?sslmode=disable' "$user" "$password" "$host" "$port" "$db"

  if ((restore_xtrace)); then
    set -x
  fi
}

validate_intended_base_url() {
  case "${BENCHDB_INTENDED_BASE_URL:-}" in
    http://*|https://*) return 0 ;;
    *)
      echo "BENCHDB_INTENDED_BASE_URL must be set to an http(s) URL" >&2
      return 1
      ;;
  esac
}

render_template_from_env() {
  local template="$1"
  shift
  python3 - "$template" "$@" <<'PY'
import os
import pathlib
import re
import sys

template = pathlib.Path(sys.argv[1])
keys = sys.argv[2:]
text = template.read_text()

for key in keys:
    value = os.environ.get(key)
    if value is None or value == "":
        raise SystemExit(f"{key} must be set to a non-empty value to render {template}")
    text = text.replace("{{" + key + "}}", value)
    text = text.replace("<" + key + ">", value)

unresolved = sorted(
    set(re.findall(r"{{[A-Z0-9_]+}}", text))
    | set(re.findall(r"<[A-Z0-9_]+>", text))
)
if unresolved:
    raise SystemExit(f"unresolved placeholders in {template}: {', '.join(unresolved)}")
sys.stdout.write(text)
PY
}

github_app_commit_auth_configured() {
  [[ -n "${BENCHDB_COMMIT_GITHUB_APP_ID:-}" || -n "${BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID:-}" || -n "${BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE:-}" ]]
}

validate_github_app_commit_auth() {
  github_app_commit_auth_configured || return 0
  local key
  for key in BENCHDB_COMMIT_GITHUB_APP_ID BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE; do
    if [[ -z "${!key:-}" ]]; then
      echo "GitHub App commit authentication requires $key" >&2
      return 1
    fi
  done
  if [[ -n "${GITHUB_API_TOKEN:-}" ]]; then
    echo "GitHub App commit authentication cannot be combined with GITHUB_API_TOKEN" >&2
    return 1
  fi
  if [[ ! -r "${BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE}" ]]; then
    echo "BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE must name a readable file" >&2
    return 1
  fi
}

render_secret_manifest() {
  local restore_xtrace=0
  case "$-" in
    *x*)
      restore_xtrace=1
      set +x
      ;;
  esac

  if ! validate_intended_base_url; then
    if ((restore_xtrace)); then
      set -x
    fi
    return 1
  fi

  local key
  for key in DB_PASSWORD DB_USERNAME; do
    if [[ -z "${!key:-}" ]]; then
      echo "$key must be set" >&2
      if ((restore_xtrace)); then
        set -x
      fi
      return 1
    fi
  done

  local benchdb_db_url
  if ! benchdb_db_url="$(build_benchdb_db_url)"; then
    if ((restore_xtrace)); then
      set -x
    fi
    return 1
  fi

  local session_secret oidc_issuer oidc_client_id oidc_client_secret oidc_enabled
  local github_app_id github_app_installation_id github_app_private_key_file github_app_enabled
  session_secret="${BENCHDB_SESSION_SECRET:-}"
  oidc_issuer="${BENCHDB_OIDC_ISSUER_URL:-}"
  oidc_client_id="${BENCHDB_OIDC_CLIENT_ID:-${GOOGLE_CLIENT_ID:-}}"
  oidc_client_secret="${BENCHDB_OIDC_CLIENT_SECRET:-${GOOGLE_CLIENT_SECRET:-}}"
  github_app_id="${BENCHDB_COMMIT_GITHUB_APP_ID:-}"
  github_app_installation_id="${BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID:-}"
  github_app_private_key_file="${BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE:-}"
  if [[ -z "$oidc_issuer" && -n "${GOOGLE_CLIENT_ID:-}" && -n "${GOOGLE_CLIENT_SECRET:-}" ]]; then
    oidc_issuer="https://accounts.google.com"
  fi

  oidc_enabled=0
  if [[ -n "${BENCHDB_OIDC_ISSUER_URL:-}" || -n "${BENCHDB_OIDC_CLIENT_ID:-}" || -n "${BENCHDB_OIDC_CLIENT_SECRET:-}" || -n "${GOOGLE_CLIENT_ID:-}" || -n "${GOOGLE_CLIENT_SECRET:-}" ]]; then
    oidc_enabled=1
  fi

  if [[ "$oidc_enabled" == "1" ]]; then
    if [[ -z "$oidc_issuer" || -z "$oidc_client_id" || -z "$oidc_client_secret" ]]; then
      echo "OIDC requires complete issuer, client id, and client secret configuration" >&2
      if ((restore_xtrace)); then
        set -x
      fi
      return 1
    fi
    if ((${#session_secret} < 32)); then
      echo "BENCHDB_SESSION_SECRET must be at least 32 characters when OIDC is enabled" >&2
      if ((restore_xtrace)); then
        set -x
      fi
      return 1
    fi
  fi

  github_app_enabled=0
  if github_app_commit_auth_configured; then
    github_app_enabled=1
    if ! validate_github_app_commit_auth; then
      if ((restore_xtrace)); then
        set -x
      fi
      return 1
    fi
  fi

  local render_status
  BENCHDB_DB_URL_RENDERED="$benchdb_db_url" \
  OIDC_ENABLED="$oidc_enabled" \
  OIDC_ISSUER_RENDERED="$oidc_issuer" \
  OIDC_CLIENT_ID_RENDERED="$oidc_client_id" \
  OIDC_CLIENT_SECRET_RENDERED="$oidc_client_secret" \
  BENCHDB_SESSION_SECRET_RENDERED="$session_secret" \
  GITHUB_APP_ENABLED="$github_app_enabled" \
  GITHUB_APP_ID_RENDERED="$github_app_id" \
  GITHUB_APP_INSTALLATION_ID_RENDERED="$github_app_installation_id" \
  python3 <<'PY'
import base64
import json
import os
import sys

data = {
    "BENCHDB_DB_URL": os.environ["BENCHDB_DB_URL_RENDERED"],
    "BENCHDB_API_TOKEN": os.environ.get("BENCHDB_API_TOKEN", ""),
}

if os.environ["GITHUB_APP_ENABLED"] == "1":
    data.update(
        {
            "BENCHDB_COMMIT_GITHUB_APP_ID": os.environ["GITHUB_APP_ID_RENDERED"],
            "BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID": os.environ["GITHUB_APP_INSTALLATION_ID_RENDERED"],
            "BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE": "/var/run/secrets/benchdb-github-app/private-key.pem",
        }
    )
else:
    data["GITHUB_API_TOKEN"] = os.environ.get("GITHUB_API_TOKEN", "")

if os.environ["OIDC_ENABLED"] == "1":
    data.update(
        {
            "BENCHDB_SESSION_SECRET": os.environ["BENCHDB_SESSION_SECRET_RENDERED"],
            "BENCHDB_OIDC_ISSUER_URL": os.environ["OIDC_ISSUER_RENDERED"],
            "BENCHDB_OIDC_CLIENT_ID": os.environ["OIDC_CLIENT_ID_RENDERED"],
            "BENCHDB_OIDC_CLIENT_SECRET": os.environ["OIDC_CLIENT_SECRET_RENDERED"],
        }
    )

encoded = {
    key: base64.b64encode(value.encode()).decode()
    for key, value in data.items()
}

json.dump(
    {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {"name": "benchdb-secret"},
        "type": "Opaque",
        "data": encoded,
    },
    sys.stdout,
    separators=(",", ":"),
)
sys.stdout.write("\n")
PY
  render_status=$?

  if ((restore_xtrace)); then
    set -x
  fi
  return "$render_status"
}

render_github_app_secret_manifest() {
  local private_key_file="${BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE:-}"
  if ! github_app_commit_auth_configured; then
    echo "complete GitHub App commit authentication is required to render its private-key secret" >&2
    return 1
  fi
  validate_github_app_commit_auth || return 1

  python3 - "$private_key_file" <<'PY'
import base64
import json
import pathlib
import sys

private_key = pathlib.Path(sys.argv[1]).read_bytes()
json.dump(
    {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {"name": "benchdb-github-app-key"},
        "type": "Opaque",
        "data": {"private-key.pem": base64.b64encode(private_key).decode()},
    },
    sys.stdout,
    separators=(",", ":"),
)
sys.stdout.write("\n")
PY
}

render_config_manifest() {
  python3 <<'PY'
import json
import os
import sys

keys = [
    "BENCHDB_ADDR",
    "BENCHDB_INTENDED_BASE_URL",
]
missing = [key for key in keys if os.environ.get(key, "") == ""]
if missing:
    raise SystemExit(f"missing or empty ConfigMap values: {', '.join(missing)}")

json.dump(
    {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": "benchdb-config", "labels": {"app": "benchdb"}},
        "data": {key: os.environ[key] for key in keys},
    },
    sys.stdout,
    separators=(",", ":"),
)
sys.stdout.write("\n")
PY
}

render_deployment_manifest() {
  local github_app_volume_mounts github_app_volumes
  if github_app_commit_auth_configured; then
    validate_github_app_commit_auth || return 1
    github_app_volume_mounts='
        volumeMounts:
          - name: benchdb-github-app-key
            mountPath: /var/run/secrets/benchdb-github-app
            readOnly: true'
    github_app_volumes='
      volumes:
        - name: benchdb-github-app-key
          secret:
            secretName: benchdb-github-app-key'
  else
    github_app_volume_mounts='GitHub App key mount disabled.'
    github_app_volumes='GitHub App key volume disabled.'
  fi
  BENCHDB_GITHUB_APP_VOLUME_MOUNTS="$github_app_volume_mounts" \
  BENCHDB_GITHUB_APP_VOLUMES="$github_app_volumes" \
    render_template_from_env k8s/benchdb-deployment.templ.yml \
      BENCHDB_SERVER_IMAGE_SPEC BENCHDB_GITHUB_APP_VOLUME_MOUNTS BENCHDB_GITHUB_APP_VOLUMES
}

render_migration_manifest() {
  render_template_from_env k8s/benchdb-db-migration.templ.yml BENCHDB_SERVER_IMAGE_SPEC
}

render_ingress_manifest() {
  validate_intended_base_url || return 1
  local intended_dns_name
  intended_dns_name="$(python3 <<'PY'
import os
from urllib.parse import urlparse

parsed = urlparse(os.environ["BENCHDB_INTENDED_BASE_URL"])
if not parsed.hostname:
    raise SystemExit("BENCHDB_INTENDED_BASE_URL must include a DNS name")
print(parsed.hostname)
PY
)" || return 1
  BENCHDB_INTENDED_DNS_NAME="$intended_dns_name" \
    render_template_from_env k8s/benchdb-cloud-ingress.templ.yml \
      CERTIFICATE_ARN \
      BENCHDB_INTENDED_DNS_NAME
}

apply_service_monitor_if_supported() {
  if kubectl get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1; then
    kubectl apply -f k8s/benchdb-service-monitor.yml || return 1
  else
    echo "skip ServiceMonitor apply; servicemonitors.monitoring.coreos.com CRD not found"
  fi
}

build_and_push() {
  set -x
  docker build -f Dockerfile.server -t "${BENCHDB_SERVER_IMAGE_NAME}" .
  docker images | grep benchdb

  docker tag "${BENCHDB_SERVER_IMAGE_NAME}:latest" "${BENCHDB_SERVER_IMAGE_SPEC}"
  aws ecr get-login-password --region us-east-2 | docker login --username AWS --password-stdin "${DOCKER_REGISTRY}"
  docker push "${BENCHDB_SERVER_IMAGE_SPEC}"
}

deploy_secrets_and_config() {
  validate_intended_base_url || return 1

  aws eks --region us-east-2 update-kubeconfig --name "${EKS_CLUSTER}"
  kubectl config set-context --current --namespace="${NAMESPACE}"

  render_secret_manifest | kubectl apply -f - || return 1
  if github_app_commit_auth_configured; then
    render_github_app_secret_manifest | kubectl apply -f - || return 1
  else
    kubectl delete secret benchdb-github-app-key --ignore-not-found=true || return 1
  fi
  render_config_manifest | kubectl apply -f - || return 1
}

run_migrations() {
  set -x

  aws eks --region us-east-2 update-kubeconfig --name "${EKS_CLUSTER}"
  kubectl config set-context --current --namespace="${NAMESPACE}"

  render_migration_manifest > _jobspec || return 1

  # Delete job first -- why is that important?
  kubectl delete --ignore-not-found=true -f _jobspec
  kubectl apply -f _jobspec

  # Note(JP): we give this 24 hours of time. Why? For those heavy migration
  # jobs that really take so long? Interesting.
  kubectl wait --for=condition=complete --timeout=86400s job/benchdb-migration

  # Get job's stdout/err. This parses this line of text to get to the pod name:
  #
  # Normal  SuccessfulCreate  10m    job-controller  Created pod: benchdb-migration-wpcp5
  export JOB_POD_NAME="$(kubectl describe job benchdb-migration | grep SuccessfulCreate | tail -n1 | awk '{print $7}')"
  kubectl logs --all-containers "${JOB_POD_NAME}"

  # Can't we do this kind of err handling in the `wait` command?
  (($(kubectl get job benchdb-migration -o jsonpath={.status.succeeded}) == "1")) \
    && exit 0 || exit 1
}

deploy() {
  set -x

  # This assumes AWS credentials that have the `--group system:masters
  # --username admin` privilege, see:
  # infra/blob/0a21e9a2eee1ea158d2a2a5d216407741feb3931/benchdb/app/stacks/eks/main.tf#L80
  # EKS_CLUSTER is currently "vd-2" for cb&cb-staging.
  aws eks --region us-east-2 update-kubeconfig --name "${EKS_CLUSTER}"

  # All of the following kubectl commands operate on a definite namespace.
  # NAMESPACE is something like "default" or "staging"
  kubectl config set-context --current --namespace="${NAMESPACE}"

  # (Re-)apply deployment using the image tagged by BENCHDB_DEPLOY_VERSION.
  render_deployment_manifest | kubectl apply -f - || return 1
  kubectl rollout restart deployment/benchdb-deployment || return 1
  kubectl apply -f k8s/benchdb-service.yml || return 1
  apply_service_monitor_if_supported || return 1

  if [[ "$EKS_CLUSTER" == "vd-2" || "$EKS_CLUSTER" == "ursa-2" ]]; then
    # (Re-)apply ALB ingress config. Note(JP): if this results in re-creation
    # of the ALB then we need to out-of-band update an A record in Route53,
    # because we do not yet use k8s externalDNS features.
    render_ingress_manifest | kubectl apply -f - || return 1
  else
    echo "skip k8s ingress patch"
  fi

  echo "Go runtime metrics are exposed at /metrics; design Grafana dashboards from current metrics"

  # Note(JP); this might be nonobvious, but `rollout status` waits for
  # progressDeadlineSeconds (see deployment manifast) before it exits non-zero.
  # See
  # https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#failed-deployment
  kubectl rollout status deployment/benchdb-deployment
}

rollback() {
  set -x
  aws eks --region us-east-2 update-kubeconfig --name "${EKS_CLUSTER}"
  kubectl config set-context --current --namespace="${NAMESPACE}"
  kubectl rollout undo deployment.v1.apps/benchdb-deployment
  kubectl rollout status deployment/benchdb-deployment
}

if [[ -z "${BENCHDB_DEPLOY_NO_DISPATCH:-}" && $# -gt 0 ]]; then
  "$@"
fi
