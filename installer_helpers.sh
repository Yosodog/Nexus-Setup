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
  : "${NEXUS_RUNTIME:=standalone}"
  : "${NEXUS_MANAGED:=false}"
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
