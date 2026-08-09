#!/usr/bin/env bash

apply_installer_defaults() {
  : "${INSTALL_PROFILE:=full}"
  : "${ENABLE_SWAP:=true}"
  : "${SWAP_SIZE_GB:=4}"
  : "${DB_HOST:=127.0.0.1}"
  : "${USE_REDIS:=false}"
  : "${REDIS_MAX_MEMORY:=}"
  : "${CERTBOT_EMAIL:=${ADMIN_EMAIL:-yourname@example.com}}"
  : "${NODE_BUILD_MAX_OLD_SPACE:=2048}"
  : "${PW_API_TOKEN:=${PW_API_KEY:-}}"
  : "${SUBS_DELIVERY_DRIVER:=http}"
  : "${SUBS_REDIS_URL:=}"
  : "${SUBS_REDIS_STREAM:=nexus:subscriptions:v1}"
  : "${SUBS_REDIS_GROUP:=nexus-ams}"
  : "${SUBS_REDIS_BLOCK_MS:=5000}"
  : "${SUBS_REDIS_READ_COUNT:=10}"
  : "${SUBS_REDIS_CLAIM_IDLE_MS:=60000}"
  : "${SUBS_REDIS_MAX_DELIVERIES:=5}"
  : "${SUBS_REDIS_HMAC_SECRET:=}"
  : "${NEXUS_RUNTIME:=standalone}"
  : "${NEXUS_MANAGED:=false}"
  : "${NEXUS_AMS_REPOSITORY:=https://github.com/Yosodog/Nexus-AMS.git}"
  : "${NEXUS_SUBS_REPOSITORY:=https://github.com/Yosodog/Nexus-AMS-Subs.git}"
  : "${NEXUS_AMS_COMMIT:=}"
  : "${NEXUS_SUBS_COMMIT:=}"
  : "${NEXUS_RELEASE_ID:=}"
  : "${ALLOW_UNPINNED_DEVELOPMENT:=false}"
}

validate_standalone_runtime() {
  local runtime="${1:-}"
  local managed="${2:-false}"

  if [[ "$runtime" != "standalone" ]]; then
    printf 'Nexus Setup supports only NEXUS_RUNTIME=standalone and rejected the configured runtime.\n' >&2
    return 1
  fi

  case "$managed" in
    true|TRUE|True|1)
      printf 'Nexus Setup cannot configure NEXUS_MANAGED=true.\n' >&2
      return 1
      ;;
  esac
}

validate_no_cloud_configuration() {
  local value

  for value in "$@"; do
    if [[ -n "$value" ]]; then
      printf 'Nexus Setup rejected hosted-only Cloud configuration.\n' >&2
      return 1
    fi
  done
}

is_true_value() {
  case "${1:-}" in
    true|TRUE|True|1|yes|YES|y|Y)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

lowercase_string() {
  printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]'
}

is_full_git_commit() {
  [[ "${1:-}" =~ ^[0-9a-fA-F]{40}$ ]] \
    && [[ "$(lowercase_string "${1:-}")" != "$(printf '0%.0s' {1..40})" ]]
}

is_safe_release_id() {
  [[ "${1:-}" =~ ^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$ ]]
}

validate_provenance_configuration() {
  local mode="${1:-noninteractive}"
  local install_app="${2:-true}"
  local install_subs="${3:-true}"
  local require_pinned=true

  if [[ "$mode" == "interactive" ]] && is_true_value "${ALLOW_UNPINNED_DEVELOPMENT:-false}"; then
    require_pinned=false
  fi

  if [[ -n "${NEXUS_AMS_COMMIT:-}" ]] && ! is_full_git_commit "$NEXUS_AMS_COMMIT"; then
    printf 'NEXUS_AMS_COMMIT must be a full 40-character commit SHA.\n' >&2
    return 1
  fi

  if [[ -n "${NEXUS_SUBS_COMMIT:-}" ]] && ! is_full_git_commit "$NEXUS_SUBS_COMMIT"; then
    printf 'NEXUS_SUBS_COMMIT must be a full 40-character commit SHA.\n' >&2
    return 1
  fi

  if [[ -n "${NEXUS_RELEASE_ID:-}" ]] && ! is_safe_release_id "$NEXUS_RELEASE_ID"; then
    printf 'NEXUS_RELEASE_ID must contain only safe release identifier characters.\n' >&2
    return 1
  fi

  if [[ "$install_subs" == "true" && "${NEXUS_RELEASE_ID:-}" == "unknown" ]]; then
    printf 'NEXUS_RELEASE_ID cannot be unknown for production and noninteractive Subs installs.\n' >&2
    return 1
  fi

  if [[ "$install_app" == "true" && "$require_pinned" == "true" && -z "${NEXUS_AMS_COMMIT:-}" ]]; then
    printf 'NEXUS_AMS_COMMIT is required for production and noninteractive AMS installs.\n' >&2
    return 1
  fi

  if [[ "$install_subs" == "true" && "$require_pinned" == "true" && -z "${NEXUS_SUBS_COMMIT:-}" ]]; then
    printf 'NEXUS_SUBS_COMMIT is required for production and noninteractive Subs installs.\n' >&2
    return 1
  fi

  if [[ "$install_subs" == "true" && "$require_pinned" == "true" && -z "${NEXUS_RELEASE_ID:-}" ]]; then
    printf 'NEXUS_RELEASE_ID is required for production and noninteractive Subs installs.\n' >&2
    return 1
  fi
}

checked_out_commit() {
  local repo_path="$1"
  local commit

  commit="$(git -C "$repo_path" rev-parse --verify HEAD^{commit} 2>/dev/null)" || return 1
  is_full_git_commit "$commit" || return 1
  printf '%s\n' "$(lowercase_string "$commit")"
}

development_release_id() {
  local commit="$1"
  is_full_git_commit "$commit" || return 1
  printf 'development-%s\n' "${commit:0:12}"
}

generate_subs_redis_hmac_secret() {
  local secret=""

  if command -v openssl >/dev/null 2>&1; then
    secret="$(openssl rand -hex 32)" || secret=""

    if [[ "$secret" =~ ^[0-9a-f]{64}$ ]]; then
      printf '%s\n' "$secret"
      return
    fi
  fi

  if command -v od >/dev/null 2>&1 && command -v tr >/dev/null 2>&1; then
    secret="$(od -An -N32 -tx1 /dev/urandom | tr -d '[:space:]')" || secret=""

    if [[ "$secret" =~ ^[0-9a-f]{64}$ ]]; then
      printf '%s\n' "$secret"
      return
    fi
  fi

  printf 'Unable to generate SUBS_REDIS_HMAC_SECRET: openssl or od and tr are required.\n' >&2
  return 1
}

subs_redis_hmac_secret_is_valid() {
  local secret="${1:-}"
  local byte_length

  byte_length="$(LC_ALL=C printf '%s' "$secret" | wc -c | tr -d '[:space:]')"
  [[ "$byte_length" -ge 32 ]]
}

read_env_value() {
  local key="$1"
  local env_file="$2"

  [[ -f "$env_file" ]] || return 0

  awk -v key="$key" '
    index($0, key "=") == 1 {
      value = substr($0, length(key) + 2)
      if (value ~ /^".*"$/ || value ~ /^\047.*\047$/) {
        value = substr(value, 2, length(value) - 2)
      }
      print value
      exit
    }
  ' "$env_file"
}

set_env_kv() {
  local key="$1"
  local value="$2"
  local env_file="$3"
  local sensitivity="${4:-plain}"

  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    local display_value="$value"

    if [[ "$sensitivity" == "secret" ]]; then
      display_value="<redacted>"
    fi

    printf '[dry-run] set %s=%s in %s\n' "$key" "$display_value" "$env_file"
    return
  fi

  local temp_file
  temp_file="$(mktemp "${TMPDIR:-/tmp}/nexus-env.XXXXXX")"

  if ! ENV_KEY="$key" ENV_VALUE="$value" awk '
    BEGIN {
      key = ENVIRON["ENV_KEY"]
      value = ENVIRON["ENV_VALUE"]
      found = 0
    }
    index($0, key "=") == 1 {
      print key "=" value
      found = 1
      next
    }
    { print }
    END {
      if (! found) {
        print key "=" value
      }
    }
  ' "$env_file" > "$temp_file"; then
    rm -f "$temp_file"
    return 1
  fi

  if ! cat "$temp_file" > "$env_file"; then
    rm -f "$temp_file"
    return 1
  fi

  rm -f "$temp_file"
}
