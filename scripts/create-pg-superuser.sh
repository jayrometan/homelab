#!/usr/bin/env bash
# scripts/create-pg-superuser.sh
#
# Creates and configures database users for the StackGres 'primary' cluster.
#
# How a production firm runs this:
#   1. Platform engineer runs this ONCE after the cluster is first provisioned.
#   2. Passwords are generated here, stored in Kubernetes Secrets, and applied
#      to Postgres via psql. Secrets are never committed to Git.
#   3. Subsequent password rotations use this script with the --rotate flag.
#   4. Application pods mount the relevant Secret as an env var or volume.
#
# Usage:
#   ./create-pg-superuser.sh                    # initial setup
#   ./create-pg-superuser.sh --rotate app_user  # rotate a single user's password
#   ./create-pg-superuser.sh --show             # print connection strings (no passwords)
#
# Requirements:
#   - kubectl configured and pointed at the right cluster/context
#   - The StackGres 'primary' cluster must be Running (check: kubectl get sgcluster -n postgres)
#   - openssl (for password generation)
#   - base64

set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────────
NAMESPACE="postgres"
CLUSTER_NAME="primary"
POOLER_SVC="${CLUSTER_NAME}-pooler"
DIRECT_SVC="${CLUSTER_NAME}"

# Users to create. Each gets a randomly generated password stored in a Secret.
USERS=(dba app_user readonly monitoring)

# ── Helpers ──────────────────────────────────────────────────────────────────
info()  { echo "[INFO]  $*"; }
warn()  { echo "[WARN]  $*" >&2; }
die()   { echo "[ERROR] $*" >&2; exit 1; }

generate_password() {
    # 32 bytes of random = 44-character base64 string.
    # tr removes +/= which can cause shell quoting issues in connection strings.
    openssl rand -base64 32 | tr -d '+/=' | head -c 32
}

get_superuser_password() {
    # StackGres auto-generates a superuser password in a Secret named after the
    # cluster. Retrieve it for running SQL commands.
    kubectl get secret "${CLUSTER_NAME}" -n "${NAMESPACE}" \
        -o jsonpath='{.data.superuser-password}' | base64 --decode
}

run_sql() {
    local sql="$1"
    local db="${2:-postgres}"
    local PG_PASS
    PG_PASS="$(get_superuser_password)"

    # Run psql inside a temporary pod in the same namespace.
    # This avoids exposing the DB to the internet (even temporarily) just for
    # admin operations. The pod is deleted immediately after use.
    kubectl run pg-admin-shell \
        --rm --restart=Never --attach --quiet \
        --image=postgres:16-alpine \
        --namespace="${NAMESPACE}" \
        --env="PGPASSWORD=${PG_PASS}" \
        --command -- psql \
            -h "${DIRECT_SVC}.${NAMESPACE}.svc.cluster.local" \
            -U postgres \
            -d "${db}" \
            -c "${sql}"
}

store_secret() {
    local user="$1"
    local password="$2"
    local secret_name="pg-credentials-${user}"

    info "Storing credentials for '${user}' in Secret '${secret_name}'"

    # Check if Secret already exists; patch if yes, create if no.
    if kubectl get secret "${secret_name}" -n "${NAMESPACE}" &>/dev/null; then
        kubectl patch secret "${secret_name}" -n "${NAMESPACE}" \
            --type=merge \
            -p "{\"stringData\":{
                \"username\":\"${user}\",
                \"password\":\"${password}\",
                \"host\":\"${POOLER_SVC}.${NAMESPACE}.svc.cluster.local\",
                \"port\":\"5432\",
                \"database\":\"app_db\",
                \"dsn\":\"postgresql://${user}:${password}@${POOLER_SVC}.${NAMESPACE}.svc.cluster.local:5432/app_db\"
            }}"
    else
        kubectl create secret generic "${secret_name}" -n "${NAMESPACE}" \
            --from-literal=username="${user}" \
            --from-literal=password="${password}" \
            --from-literal=host="${POOLER_SVC}.${NAMESPACE}.svc.cluster.local" \
            --from-literal=port="5432" \
            --from-literal=database="app_db" \
            --from-literal=dsn="postgresql://${user}:${password}@${POOLER_SVC}.${NAMESPACE}.svc.cluster.local:5432/app_db"
    fi
}

# ── Commands ──────────────────────────────────────────────────────────────────
cmd_setup() {
    info "=== StackGres '${CLUSTER_NAME}' — initial user setup ==="

    # Verify cluster is running
    local phase
    phase=$(kubectl get sgcluster "${CLUSTER_NAME}" -n "${NAMESPACE}" \
        -o jsonpath='{.status.conditions[?(@.type=="PodRequirementsAreMet")].status}' 2>/dev/null || echo "Unknown")
    if [[ "${phase}" != "True" ]]; then
        warn "Cluster may not be fully ready (PodRequirementsAreMet=${phase}). Proceeding anyway."
    fi

    for user in "${USERS[@]}"; do
        local password
        password="$(generate_password)"

        info "Setting password for user '${user}'..."
        run_sql "ALTER ROLE ${user} PASSWORD '${password}';" "postgres"

        store_secret "${user}" "${password}"
        info "User '${user}' configured. Secret: pg-credentials-${user}"
    done

    info ""
    info "=== Setup complete ==="
    info ""
    info "Application connection:"
    info "  Host:     ${POOLER_SVC}.${NAMESPACE}.svc.cluster.local"
    info "  Port:     5432 (PgBouncer → Postgres)"
    info "  Database: app_db"
    info "  User:     app_user"
    info "  Secret:   kubectl get secret pg-credentials-app_user -n ${NAMESPACE}"
    info ""
    info "DBA connection (direct Postgres, not via pooler):"
    info "  Host:     ${DIRECT_SVC}.${NAMESPACE}.svc.cluster.local"
    info "  Port:     5432"
    info "  User:     dba"
    info "  Secret:   kubectl get secret pg-credentials-dba -n ${NAMESPACE}"
    info ""
    info "Get connection DSN:"
    info "  kubectl get secret pg-credentials-app_user -n ${NAMESPACE} -o jsonpath='{.data.dsn}' | base64 -d"
}

cmd_rotate() {
    local user="${1:-}"
    [[ -z "${user}" ]] && die "Usage: $0 --rotate <username>"

    local password
    password="$(generate_password)"

    info "Rotating password for '${user}'..."
    run_sql "ALTER ROLE ${user} PASSWORD '${password}';" "postgres"
    store_secret "${user}" "${password}"
    info "Password rotated. Secret updated: pg-credentials-${user}"
}

cmd_show() {
    info "=== Connection endpoints (no passwords) ==="
    info "Pooler (apps):      ${POOLER_SVC}.${NAMESPACE}.svc.cluster.local:5432"
    info "Primary (DBA/dump): ${DIRECT_SVC}.${NAMESPACE}.svc.cluster.local:5432"
    info "Replicas (read):    ${CLUSTER_NAME}-replicas.${NAMESPACE}.svc.cluster.local:5432"
    info ""
    info "Secrets:"
    for user in "${USERS[@]}"; do
        echo "  pg-credentials-${user} (namespace: ${NAMESPACE})"
    done
    info ""
    info "Get a DSN (example for app_user):"
    info "  kubectl get secret pg-credentials-app_user -n ${NAMESPACE} \\"
    info "    -o jsonpath='{.data.dsn}' | base64 -d"
}

# ── Main ─────────────────────────────────────────────────────────────────────
case "${1:-setup}" in
    --rotate) cmd_rotate "${2:-}" ;;
    --show)   cmd_show ;;
    setup|"") cmd_setup ;;
    *) die "Unknown command: $1. Use: setup, --rotate <user>, --show" ;;
esac
