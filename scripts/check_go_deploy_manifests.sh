#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
cleanup() {
	rm -rf "$tmp"
}
trap cleanup EXIT

export DOCKER_REGISTRY="registry.example"
export BENCHDB_DEPLOY_VERSION="test"
export BENCHDB_SERVER_IMAGE_NAME="benchdb-server"
export BENCHDB_SERVER_IMAGE_SPEC="${DOCKER_REGISTRY}/${BENCHDB_SERVER_IMAGE_NAME}:${BENCHDB_DEPLOY_VERSION}"
export BENCHDB_ADDR=":8080"
export BENCHDB_INTENDED_BASE_URL="https://benchdb.example.com"
export CERTIFICATE_ARN="arn:aws:acm:us-east-1:000000000000:certificate/example"
export DB_NAME="benchdb"
export DB_HOST="postgres.example"
export DB_PORT="5432"

render_prod_deployment="$tmp/prod-deployment.yml"
render_app_deployment="$tmp/app-deployment.yml"
render_prod_migration="$tmp/prod-migration.yml"
render_prod_ingress="$tmp/prod-ingress.yml"
render_config="$tmp/config.yml"
github_app_key_file="$tmp/github-app-private-key.pem"
failures=0

printf '%s\n' 'test-github-app-private-key' > "$github_app_key_file"

export BENCHDB_DEPLOY_NO_DISPATCH=1
. "$root/scripts/go_deploy_runtime.sh"
unset BENCHDB_DEPLOY_NO_DISPATCH

record_failure() {
	local message="$1"
	echo "$message" >&2
	failures=$((failures + 1))
}

if ! render_deployment_manifest > "$render_prod_deployment"; then
	record_failure "failed to render production deployment manifest"
	: > "$render_prod_deployment"
fi
if ! (
	export BENCHDB_COMMIT_GITHUB_APP_ID='12345'
	export BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID='42'
	export BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE="$github_app_key_file"
	render_deployment_manifest
) > "$render_app_deployment"; then
	record_failure "failed to render GitHub App deployment manifest"
	: > "$render_app_deployment"
fi
if ! render_migration_manifest > "$render_prod_migration"; then
	record_failure "failed to render production migration manifest"
	: > "$render_prod_migration"
fi
if ! render_ingress_manifest > "$render_prod_ingress"; then
	record_failure "failed to render production ingress manifest"
	: > "$render_prod_ingress"
fi
if ! render_config_manifest > "$render_config"; then
	record_failure "failed to render config manifest"
	: > "$render_config"
fi
if BENCHDB_SERVER_IMAGE_SPEC= render_deployment_manifest >/dev/null 2>&1; then
	record_failure "empty BENCHDB_SERVER_IMAGE_SPEC render unexpectedly succeeded"
fi
if BENCHDB_SERVER_IMAGE_SPEC= render_migration_manifest >/dev/null 2>&1; then
	record_failure "empty BENCHDB_SERVER_IMAGE_SPEC render unexpectedly succeeded"
fi
if CERTIFICATE_ARN= render_ingress_manifest >/dev/null 2>&1; then
	record_failure "empty CERTIFICATE_ARN render unexpectedly succeeded"
fi
if BENCHDB_INTENDED_BASE_URL= render_config_manifest >/dev/null 2>&1; then
	record_failure "empty BENCHDB_INTENDED_BASE_URL config render unexpectedly succeeded"
fi
if (
	export DOCKER_REGISTRY="registry.example"
	unset BENCHDB_DEPLOY_VERSION
	export BENCHDB_DEPLOY_NO_DISPATCH=1
	. "$root/scripts/go_deploy_runtime.sh"
) >/dev/null 2>&1; then
	record_failure "missing BENCHDB_DEPLOY_VERSION unexpectedly allowed deploy helper load"
fi
require_contains() {
	local file="$1"
	local pattern="$2"
	if ! grep -Fq "$pattern" "$file"; then
		record_failure "missing expected pattern in $file: $pattern"
	fi
}

require_line() {
	local file="$1"
	local regex="$2"
	if ! grep -Eq "$regex" "$file"; then
		record_failure "missing expected line in $file: $regex"
	fi
}

require_absent() {
	local file="$1"
	local pattern="$2"
	if grep -Fq "$pattern" "$file"; then
		record_failure "unexpected pattern in $file: $pattern"
	fi
}

require_missing() {
	local path="$1"
	if [[ -e "$path" ]]; then
		record_failure "unexpected path exists: $path"
	fi
}

require_secret_key() {
	local file="$1"
	local key="$2"
	if ! python3 - "$file" "$key" <<'PY'
import json
import sys

obj = json.load(open(sys.argv[1]))
data = obj.get("data")
if obj.get("kind") != "Secret" or not isinstance(data, dict):
    raise SystemExit("rendered object is not a Kubernetes Secret with data")
if sys.argv[2] not in data:
    raise SystemExit(f"missing secret key: {sys.argv[2]}")
PY
	then
		record_failure "missing rendered secret key in $file: $key"
	fi
}

require_secret_absent_key() {
	local file="$1"
	local key="$2"
	if ! python3 - "$file" "$key" <<'PY'
import json
import sys

data = json.load(open(sys.argv[1])).get("data", {})
if sys.argv[2] in data:
    raise SystemExit(f"unexpected secret key: {sys.argv[2]}")
PY
	then
		record_failure "unexpected rendered secret key in $file: $key"
	fi
}

require_secret_value() {
	local file="$1"
	local key="$2"
	local value="$3"
	if ! python3 - "$file" "$key" "$value" <<'PY'
import base64
import json
import sys

data = json.load(open(sys.argv[1])).get("data", {})
raw = data.get(sys.argv[2])
if raw is None:
    raise SystemExit(f"missing value for {sys.argv[2]}")
if base64.b64decode(raw).decode() != sys.argv[3]:
    raise SystemExit(f"unexpected value for {sys.argv[2]}")
PY
	then
		record_failure "unexpected rendered secret value in $file: $key"
	fi
}

require_config_value() {
	local file="$1"
	local key="$2"
	local value="$3"
	if ! python3 - "$file" "$key" "$value" <<'PY'
import json
import sys

data = json.load(open(sys.argv[1])).get("data", {})
if data.get(sys.argv[2]) != sys.argv[3]:
    raise SystemExit(f"unexpected value for {sys.argv[2]}")
PY
	then
		record_failure "unexpected rendered config value in $file: $key"
	fi
}

require_yaml_kinds() {
	local file="$1"
	shift
	if ! python3 - "$file" "$@" <<'PY'
import json
import sys

path = sys.argv[1]
expected = list(sys.argv[2:])

try:
    with open(path, encoding="utf-8") as f:
        obj = json.load(f)
    kind = obj.get("kind")
    kinds = [kind] if kind else []
    if kinds != expected:
        raise SystemExit(f"expected manifest kinds {expected}, got {kinds}")
    raise SystemExit(0)
except json.JSONDecodeError:
    pass

docs = []
current = []

def flush():
    if current:
        docs.append("\n".join(current))
        current.clear()

with open(path, encoding="utf-8") as f:
    for line in f:
        if line.strip() == "---":
            flush()
        else:
            current.append(line.rstrip("\n"))
flush()

kinds = []
for doc in docs:
    kind = None
    for line in doc.splitlines():
        if line.startswith("kind:"):
            kind = line.split(":", 1)[1].strip().strip('"')
            break
    if kind:
        kinds.append(kind)

if kinds != expected:
    raise SystemExit(f"expected manifest kinds {expected}, got {kinds}")
PY
	then
		record_failure "unexpected YAML document kinds in $file"
	fi
}

write_key_set() {
	printf '%s\n' "$@" | sort
}

json_map_keys() {
	python3 - "$1" "$2" <<'PY'
import json
import sys

obj = json.load(open(sys.argv[1]))
for key in sorted(obj.get(sys.argv[2], {}).keys()):
    print(key)
PY
}

require_key_sets_equal() {
	local label="$1"
	local left="$2"
	local right="$3"
	if ! diff -u "$left" "$right" >/dev/null; then
		record_failure "key set mismatch for $label"
	fi
}

require_log_order() {
	local file="$1"
	local first="$2"
	local second="$3"
	if ! python3 - "$file" "$first" "$second" <<'PY'
import sys

lines = open(sys.argv[1], encoding="utf-8").read().splitlines()
first = next((i for i, line in enumerate(lines) if sys.argv[2] in line), None)
second = next((i for i, line in enumerate(lines) if sys.argv[3] in line), None)
if first is None or second is None or first >= second:
    raise SystemExit(1)
PY
	then
		record_failure "unexpected command order in $file: $first before $second"
	fi
}

require_service_apply_before_ingress_branch() {
	local file="$1"
	if ! python3 - "$file" <<'PY'
import sys

lines = open(sys.argv[1], encoding="utf-8").read().splitlines()
deploy_start = next((i for i, line in enumerate(lines) if line.startswith("deploy() {")), None)
if deploy_start is None:
    raise SystemExit("deploy() not found")
deploy_lines = lines[deploy_start:]
deploy_end = next((i for i, line in enumerate(deploy_lines[1:], 1) if line == "}"), None)
if deploy_end is None:
    raise SystemExit("deploy() end not found")
deploy_lines = deploy_lines[:deploy_end]
service_apply = next((i for i, line in enumerate(deploy_lines) if "kubectl apply -f k8s/benchdb-service.yml" in line), None)
ingress_branch = next((i for i, line in enumerate(deploy_lines) if 'EKS_CLUSTER" == "vd-2"' in line), None)
if service_apply is None:
    raise SystemExit("service apply not found in deploy()")
if ingress_branch is None:
    raise SystemExit("ingress branch not found in deploy()")
if service_apply > ingress_branch:
    raise SystemExit("service apply must happen before ingress-only branch")
PY
	then
		record_failure "k8s service is not applied unconditionally before ingress branch"
	fi
}

render_secret_case() {
	local outfile="$1"
	local profile="$2"
	(
		export DB_PASSWORD='pa:ss/word'
		export DB_USERNAME='benchdb-user'
		export DB_NAME='benchdb'
		export DB_HOST='postgres.example'
		export DB_PORT='5432'
		export BENCHDB_API_TOKEN='dummy-static-token'
		export GITHUB_API_TOKEN='dummy-github-token'
		export BENCHDB_INTENDED_BASE_URL='https://benchdb.example.com'
		unset BENCHDB_OIDC_ISSUER_URL BENCHDB_OIDC_CLIENT_ID BENCHDB_OIDC_CLIENT_SECRET
		unset BENCHDB_SESSION_SECRET GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET
		unset BENCHDB_COMMIT_GITHUB_APP_ID BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID
		unset BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE
		case "$profile" in
			no_oidc) ;;
			no_github)
				unset GITHUB_API_TOKEN
				;;
			github_app)
				unset GITHUB_API_TOKEN
				export BENCHDB_COMMIT_GITHUB_APP_ID='12345'
				export BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID='42'
				export BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE="$github_app_key_file"
				;;
			google_oidc)
				export GOOGLE_CLIENT_ID='legacy-client'
				export GOOGLE_CLIENT_SECRET='legacy-secret'
				export BENCHDB_SESSION_SECRET='session-secret-with-at-least-thirty-two-bytes'
				;;
			native_oidc)
				export BENCHDB_OIDC_ISSUER_URL='https://issuer.example'
				export BENCHDB_OIDC_CLIENT_ID='native-client'
				export BENCHDB_OIDC_CLIENT_SECRET='native-secret'
				export BENCHDB_SESSION_SECRET='session-secret-with-at-least-thirty-two-bytes'
				;;
			oidc_no_session)
				export BENCHDB_OIDC_ISSUER_URL='https://issuer.example'
				export BENCHDB_OIDC_CLIENT_ID='client'
				export BENCHDB_OIDC_CLIENT_SECRET='secret'
				;;
			partial_oidc)
				export BENCHDB_OIDC_CLIENT_ID='client-only'
				;;
			short_session)
				export BENCHDB_OIDC_ISSUER_URL='https://issuer.example'
				export BENCHDB_OIDC_CLIENT_ID='client'
				export BENCHDB_OIDC_CLIENT_SECRET='secret'
				export BENCHDB_SESSION_SECRET='short'
				;;
			missing_base_url)
				unset BENCHDB_INTENDED_BASE_URL
				;;
			*)
				echo "unknown secret render profile: $profile" >&2
				exit 1
				;;
		esac
		render_secret_manifest
	) > "$outfile"
}

render_github_app_key_case() {
	local outfile="$1"
	(
		export BENCHDB_COMMIT_GITHUB_APP_ID='12345'
		export BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID='42'
		export BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE="$github_app_key_file"
		render_github_app_secret_manifest
	) > "$outfile"
}

exercise_deploy_secrets_case() {
	local outfile="$1"
	local profile="$2"
	(
		export DB_PASSWORD='pa:ss/word'
		export DB_USERNAME='benchdb-user'
		export DB_NAME='benchdb'
		export DB_HOST='postgres.example'
		export DB_PORT='5432'
		export BENCHDB_API_TOKEN='dummy-static-token'
		export BENCHDB_INTENDED_BASE_URL='https://benchdb.example.com'
		export EKS_CLUSTER='cluster.example'
		export NAMESPACE='default'
		unset BENCHDB_COMMIT_GITHUB_APP_ID BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID
		unset BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE
		case "$profile" in
			static)
			export GITHUB_API_TOKEN='dummy-github-token'
			;;
			github_app)
			unset GITHUB_API_TOKEN
			export BENCHDB_COMMIT_GITHUB_APP_ID='12345'
			export BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID='42'
			export BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE="$github_app_key_file"
			;;
		esac
		aws() {
			printf 'aws %s\n' "$*" >> "$outfile"
		}
		kubectl() {
			printf 'kubectl %s\n' "$*" >> "$outfile"
			if [[ "$1" == apply ]]; then
				command cat >/dev/null
			fi
		}
		deploy_secrets_and_config
	)
}

exercise_runtime_deploy_case() {
	local outfile="$1"
	(
		export EKS_CLUSTER='cluster.example'
		export NAMESPACE='default'
		aws() {
			printf 'aws %s\n' "$*" >> "$outfile"
		}
		render_deployment_manifest() {
			printf '%s\n' 'apiVersion: apps/v1' 'kind: Deployment' 'metadata:' '  name: benchdb-deployment'
		}
		apply_service_monitor_if_supported() {
			printf '%s\n' 'service monitor handled' >> "$outfile"
		}
		kubectl() {
			if [[ "$*" == 'apply -f -' ]]; then
				local payload
				payload=$(command cat)
				if grep -Fq 'kind: Deployment' <<< "$payload"; then
					printf '%s\n' 'kubectl apply deployment/benchdb-deployment' >> "$outfile"
				else
					printf '%s\n' 'kubectl apply stdin' >> "$outfile"
				fi
				return
			fi
			printf 'kubectl %s\n' "$*" >> "$outfile"
		}
		deploy
	) >/dev/null 2>&1
}

require_missing "$root/cmd/benchdb-server"

require_absent "$render_prod_deployment" "gunicorn"
require_absent "$render_prod_deployment" "gunicorn-port"

require_contains "$render_prod_deployment" "image: \"${BENCHDB_SERVER_IMAGE_SPEC}\""
require_absent "$render_prod_deployment" "BENCHDB_WEBAPP_IMAGE_SPEC"
require_line "$render_prod_deployment" '^[[:space:]]*replicas:[[:space:]]*2$'
require_line "$render_prod_deployment" '^[[:space:]]*type:[[:space:]]*RollingUpdate$'
require_line "$render_prod_deployment" '^[[:space:]]*maxSurge:[[:space:]]*25%$'
require_line "$render_prod_deployment" '^[[:space:]]*maxUnavailable:[[:space:]]*25%$'
require_absent "$render_prod_deployment" "single-replica"
require_absent "$render_prod_deployment" "process-local"
require_contains "$render_prod_deployment" "name: http"
require_contains "$render_prod_deployment" "containerPort: 8080"
require_contains "$render_prod_deployment" "startupProbe:"
require_contains "$render_prod_deployment" "livenessProbe:"
require_contains "$render_prod_deployment" "readinessProbe:"
require_absent "$render_prod_deployment" "benchdb-github-app-key"
require_line "$render_prod_deployment" '^[[:space:]]*path:[[:space:]]*/api/ping$'
require_contains "$render_prod_deployment" "port: http"
require_absent "$render_prod_deployment" "{{"
require_yaml_kinds "$render_prod_deployment" "Deployment"
require_contains "$render_app_deployment" "mountPath: /var/run/secrets/benchdb-github-app"
require_contains "$render_app_deployment" "secretName: benchdb-github-app-key"
require_yaml_kinds "$render_app_deployment" "Deployment"

require_yaml_kinds "$root/k8s/benchdb-service.yml" "Service"
require_line "$root/k8s/benchdb-service-monitor.yml" '^[[:space:]]*path:[[:space:]]*/metrics$'
require_yaml_kinds "$root/k8s/benchdb-service-monitor.yml" "ServiceMonitor"
require_line "$render_prod_ingress" '^[[:space:]]*alb.ingress.kubernetes.io/healthcheck-path:[[:space:]]*/api/ping$'
require_contains "$render_prod_ingress" "alb.ingress.kubernetes.io/actions.benchdb-metrics-deny"
require_line "$render_prod_ingress" '^[[:space:]]*- path:[[:space:]]*/metrics$'
require_contains "$render_prod_ingress" "name: benchdb-metrics-deny"
require_contains "$render_prod_ingress" "name: use-annotation"
require_absent "$render_prod_ingress" "gunicorn"
require_absent "$render_prod_ingress" "{{"
require_absent "$render_prod_ingress" "<BENCHDB_INTENDED_DNS_NAME>"
require_absent "$render_prod_ingress" "<CERTIFICATE_ARN>"
require_yaml_kinds "$render_prod_ingress" "Ingress"

require_contains "$render_prod_migration" "image: \"${BENCHDB_SERVER_IMAGE_SPEC}\""
require_contains "$render_prod_migration" 'command: ["/usr/local/bin/benchdb", "migrate"]'
require_absent "$render_prod_migration" "BENCHDB_SERVER_IMAGE_SPEC"
require_absent "$render_prod_migration" "BENCHDB_WEBAPP_IMAGE_SPEC"
require_absent "$render_prod_migration" "{{"
require_yaml_kinds "$render_prod_migration" "Job"

require_config_value "$render_config" "BENCHDB_ADDR" ":8080"
require_config_value "$render_config" "BENCHDB_INTENDED_BASE_URL" "https://benchdb.example.com"
for stale_config_key in APPLICATION_NAME BENCHMARKS_DATA_PUBLIC DB_NAME DB_HOST DB_PORT DISTRIBUTION_COMMITS SVS_TYPE; do
	require_absent "$render_config" "\"${stale_config_key}\""
done
require_yaml_kinds "$render_config" "ConfigMap"
require_absent "$render_config" "FLASK_APP"
write_key_set \
	"BENCHDB_ADDR" \
	"BENCHDB_INTENDED_BASE_URL" \
	> "$tmp/config-expected-keys"
json_map_keys "$render_config" "data" > "$tmp/config-render-keys"
require_key_sets_equal "benchdb config contract" "$tmp/config-expected-keys" "$tmp/config-render-keys"

require_missing "$root/ci/minikube"
require_missing "$root/benchdb-config.yml"
require_missing "$root/benchdb-secret.yml"
require_missing "$root/.buildkite"
require_service_apply_before_ingress_branch "$root/scripts/go_deploy_runtime.sh"
require_missing "$root/k8s/benchdb-grafana-dashboard-configmap.template.yml"
require_missing "$root/k8s/kube-prometheus/benchdb-grafana-dashboard.json"
require_missing "$root/k8s/kube-prometheus"

static_deploy_log="$tmp/static-deploy.log"
if exercise_deploy_secrets_case "$static_deploy_log" static; then
	require_contains "$static_deploy_log" "kubectl delete secret benchdb-github-app-key --ignore-not-found=true"
	require_absent "$static_deploy_log" "kubectl rollout restart deployment/benchdb-deployment"
else
	record_failure "static secret deployment exercise failed"
fi

github_app_deploy_log="$tmp/github-app-deploy.log"
if exercise_deploy_secrets_case "$github_app_deploy_log" github_app; then
	require_absent "$github_app_deploy_log" "kubectl delete secret benchdb-github-app-key"
	require_absent "$github_app_deploy_log" "kubectl rollout restart deployment/benchdb-deployment"
else
	record_failure "GitHub App secret deployment exercise failed"
fi

runtime_deploy_log="$tmp/runtime-deploy.log"
if exercise_runtime_deploy_case "$runtime_deploy_log"; then
	require_contains "$runtime_deploy_log" "kubectl rollout restart deployment/benchdb-deployment"
	require_log_order "$runtime_deploy_log" \
		"kubectl apply deployment/benchdb-deployment" \
		"kubectl rollout restart deployment/benchdb-deployment"
else
	record_failure "runtime deployment exercise failed"
fi

secret_no_oidc="$tmp/secret-no-oidc.json"
if render_secret_case "$secret_no_oidc" no_oidc; then
	require_secret_key "$secret_no_oidc" "BENCHDB_DB_URL"
	require_secret_value "$secret_no_oidc" "BENCHDB_DB_URL" "postgres://benchdb-user:pa%3Ass%2Fword@postgres.example:5432/benchdb?sslmode=disable"
	require_secret_value "$secret_no_oidc" "BENCHDB_API_TOKEN" "dummy-static-token"
	for stale_secret_key in DB_PASSWORD DB_USERNAME SECRET_KEY REGISTRATION_KEY GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET; do
		require_secret_absent_key "$secret_no_oidc" "$stale_secret_key"
	done
	require_secret_absent_key "$secret_no_oidc" "BENCHDB_OIDC_ISSUER_URL"
	require_secret_absent_key "$secret_no_oidc" "BENCHDB_OIDC_CLIENT_ID"
	require_secret_absent_key "$secret_no_oidc" "BENCHDB_OIDC_CLIENT_SECRET"
	require_secret_absent_key "$secret_no_oidc" "BENCHDB_SESSION_SECRET"
else
	record_failure "no-OIDC secret render failed"
fi

secret_no_github="$tmp/secret-no-github.json"
if render_secret_case "$secret_no_github" no_github; then
	require_secret_value "$secret_no_github" "GITHUB_API_TOKEN" ""
else
	record_failure "no-GitHub-token secret render failed"
fi

secret_github_app="$tmp/secret-github-app.json"
if render_secret_case "$secret_github_app" github_app; then
	require_secret_absent_key "$secret_github_app" "GITHUB_API_TOKEN"
	require_secret_value "$secret_github_app" "BENCHDB_COMMIT_GITHUB_APP_ID" "12345"
	require_secret_value "$secret_github_app" "BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID" "42"
	require_secret_value "$secret_github_app" "BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE" "/var/run/secrets/benchdb-github-app/private-key.pem"
else
	record_failure "GitHub App runtime secret render failed"
fi

github_app_key_secret="$tmp/github-app-key-secret.json"
if render_github_app_key_case "$github_app_key_secret"; then
	require_secret_value "$github_app_key_secret" "private-key.pem" "test-github-app-private-key
"
else
	record_failure "GitHub App private-key secret render failed"
fi

secret_google_oidc="$tmp/secret-google-oidc.json"
if render_secret_case "$secret_google_oidc" google_oidc; then
	require_secret_absent_key "$secret_google_oidc" "GOOGLE_CLIENT_ID"
	require_secret_absent_key "$secret_google_oidc" "GOOGLE_CLIENT_SECRET"
	require_secret_value "$secret_google_oidc" "BENCHDB_OIDC_CLIENT_ID" "legacy-client"
	require_secret_value "$secret_google_oidc" "BENCHDB_OIDC_CLIENT_SECRET" "legacy-secret"
	require_secret_value "$secret_google_oidc" "BENCHDB_OIDC_ISSUER_URL" "https://accounts.google.com"
	require_secret_value "$secret_google_oidc" "BENCHDB_SESSION_SECRET" "session-secret-with-at-least-thirty-two-bytes"
else
	record_failure "legacy Google OIDC secret render failed"
fi

secret_native_oidc="$tmp/secret-native-oidc.json"
if render_secret_case "$secret_native_oidc" native_oidc; then
	require_secret_value "$secret_native_oidc" "BENCHDB_OIDC_ISSUER_URL" "https://issuer.example"
	require_secret_value "$secret_native_oidc" "BENCHDB_OIDC_CLIENT_ID" "native-client"
	require_secret_value "$secret_native_oidc" "BENCHDB_OIDC_CLIENT_SECRET" "native-secret"
	require_secret_absent_key "$secret_native_oidc" "GOOGLE_CLIENT_ID"
	require_secret_absent_key "$secret_native_oidc" "GOOGLE_CLIENT_SECRET"
	require_secret_value "$secret_native_oidc" "BENCHDB_SESSION_SECRET" "session-secret-with-at-least-thirty-two-bytes"
else
	record_failure "native OIDC secret render failed"
fi
json_map_keys "$secret_native_oidc" "data" > "$tmp/secret-render-keys"
write_key_set \
	"BENCHDB_API_TOKEN" \
	"BENCHDB_DB_URL" \
	"BENCHDB_OIDC_CLIENT_ID" \
	"BENCHDB_OIDC_CLIENT_SECRET" \
	"BENCHDB_OIDC_ISSUER_URL" \
	"BENCHDB_SESSION_SECRET" \
	"GITHUB_API_TOKEN" \
	> "$tmp/secret-expected-keys"
require_key_sets_equal "benchdb secret contract" "$tmp/secret-expected-keys" "$tmp/secret-render-keys"

if render_secret_case "$tmp/secret-partial-oidc.json" partial_oidc 2>/dev/null; then
	record_failure "partial OIDC render unexpectedly succeeded"
fi
if render_secret_case "$tmp/secret-oidc-no-session.json" oidc_no_session 2>/dev/null; then
	record_failure "OIDC without explicit BENCHDB_SESSION_SECRET render unexpectedly succeeded"
fi
if render_secret_case "$tmp/secret-short-session.json" short_session 2>/dev/null; then
	record_failure "short session secret render unexpectedly succeeded"
fi
if render_secret_case "$tmp/secret-missing-base-url.json" missing_base_url 2>/dev/null; then
	record_failure "missing BENCHDB_INTENDED_BASE_URL render unexpectedly succeeded"
fi

if ((failures > 0)); then
	echo "go deploy manifest check failed with ${failures} failure(s)" >&2
	exit 1
fi

echo "go deploy manifest check OK"
