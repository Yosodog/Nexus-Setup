#!/usr/bin/env bash

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=../installer_helpers.sh
source "${PROJECT_ROOT}/installer_helpers.sh"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local message="$3"

  [[ "$haystack" == *"$needle"* ]] || fail "$message"
}

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  local message="$3"

  [[ "$haystack" != *"$needle"* ]] || fail "$message"
}

(
  unset INSTALL_PROFILE ENABLE_SWAP SWAP_SIZE_GB DB_HOST USE_REDIS REDIS_MAX_MEMORY
  unset CERTBOT_EMAIL NODE_BUILD_MAX_OLD_SPACE PW_API_TOKEN SUBS_DELIVERY_DRIVER
  unset SUBS_REDIS_URL SUBS_REDIS_STREAM SUBS_REDIS_GROUP SUBS_REDIS_BLOCK_MS
  unset SUBS_REDIS_READ_COUNT SUBS_REDIS_CLAIM_IDLE_MS SUBS_REDIS_MAX_DELIVERIES
  unset NEXUS_RUNTIME NEXUS_MANAGED

  ADMIN_EMAIL='legacy-admin@example.test'
  PW_API_KEY='synthetic-pw-key'
  apply_installer_defaults

  [[ "$INSTALL_PROFILE" == 'full' ]] || fail 'legacy install profile does not default to full'
  [[ "$ENABLE_SWAP" == 'true' && "$SWAP_SIZE_GB" == '4' ]] || fail 'legacy swap defaults changed'
  [[ "$DB_HOST" == '127.0.0.1' ]] || fail 'legacy database host does not default locally'
  [[ "$USE_REDIS" == 'false' && -z "$REDIS_MAX_MEMORY" ]] || fail 'legacy Redis defaults changed'
  [[ "$CERTBOT_EMAIL" == "$ADMIN_EMAIL" ]] || fail 'legacy Certbot email is not preserved'
  [[ "$PW_API_TOKEN" == "$PW_API_KEY" ]] || fail 'Subs key does not default to the AMS key'
  [[ "$SUBS_DELIVERY_DRIVER" == 'http' ]] || fail 'legacy delivery does not default to HTTP'
  [[ "$NEXUS_RUNTIME" == 'standalone' ]] || fail 'legacy runtime does not default to standalone'
  [[ "$NEXUS_MANAGED" == 'false' ]] || fail 'legacy runtime defaults to managed'
)

validate_standalone_runtime 'standalone' 'false' || fail 'standalone runtime is rejected'

if validate_standalone_runtime 'hosted-tenant' 'true' >/dev/null 2>&1; then
  fail 'hosted-tenant runtime is accepted'
fi

if validate_standalone_runtime 'world-writer' 'false' >/dev/null 2>&1; then
  fail 'world-writer runtime is accepted'
fi

validate_no_cloud_configuration '' '' || fail 'empty hosted-only configuration is rejected'

if validate_no_cloud_configuration '01JTESTTENANT' '' >/dev/null 2>&1; then
  fail 'a Cloud tenant identity is accepted by the standalone installer'
fi

temporary_directory="$(mktemp -d)"
trap 'rm -rf "$temporary_directory"' EXIT
environment_file="${temporary_directory}/service.env"
touch "$environment_file"

DRY_RUN=true
synthetic_secret='synthetic-secret-that-must-not-be-logged'
dry_run_output="$(set_env_kv 'NEXUS_API_TOKEN' "$synthetic_secret" "$environment_file" 'secret')"
assert_contains "$dry_run_output" 'NEXUS_API_TOKEN=<redacted>' 'dry-run credential is not redacted'
assert_not_contains "$dry_run_output" "$synthetic_secret" 'dry-run output contains a credential'
[[ ! -s "$environment_file" ]] || fail 'dry-run modified an environment file'

DRY_RUN=false
replacement_value='replacement&value|with\backslash'
set_env_kv 'NEXUS_API_TOKEN' "$synthetic_secret" "$environment_file" 'secret'
set_env_kv 'NEXUS_API_TOKEN' "$replacement_value" "$environment_file" 'secret'
grep -Fqx "NEXUS_API_TOKEN=${replacement_value}" "$environment_file" \
  || fail 'environment value was not replaced exactly'
[[ "$(grep -Fc 'NEXUS_API_TOKEN=' "$environment_file")" -eq 1 ]] \
  || fail 'environment replacement created duplicate keys'

printf 'NEXUS_RUNTIME="standalone"\nNEXUS_MANAGED=false\n' > "${temporary_directory}/read.env"
[[ "$(read_env_value 'NEXUS_RUNTIME' "${temporary_directory}/read.env")" == 'standalone' ]] \
  || fail 'quoted runtime could not be read from an existing AMS environment'

prepare_config() {
  local destination="$1"
  local app_path="$2"

  cp "${PROJECT_ROOT}/install.env" "$destination"
  printf '\nAPP_PATH="%s"\nSUBS_PATH="%s/subs"\n' "$app_path" "$temporary_directory" >> "$destination"
}

standalone_directory="${temporary_directory}/standalone"
mkdir -p "$standalone_directory"
prepare_config "${standalone_directory}/install.env" "${standalone_directory}/app"
standalone_output="$(cd "$standalone_directory" && bash "${PROJECT_ROOT}/install_nexus.sh" --check-config)"
assert_contains "$standalone_output" 'Configuration is valid for a standalone Nexus AMS installation.' \
  'standalone config check did not pass'
assert_not_contains "$standalone_output" 'StrongPasswordHere!' 'config check logged the database password'
assert_not_contains "$standalone_output" 'your_nexus_api_token_here' 'config check logged an API credential'

for profile in full app-web-subs-remote-db web-only db-only subs-only; do
  profile_directory="${temporary_directory}/profile-${profile}"
  mkdir -p "$profile_directory"
  prepare_config "${profile_directory}/install.env" "${profile_directory}/app"
  printf 'INSTALL_PROFILE="%s"\n' "$profile" >> "${profile_directory}/install.env"
  (cd "$profile_directory" && bash "${PROJECT_ROOT}/install_nexus.sh" --check-config >/dev/null) \
    || fail "${profile} profile failed configuration validation"
done

invalid_profile_directory="${temporary_directory}/profile-invalid"
mkdir -p "$invalid_profile_directory"
prepare_config "${invalid_profile_directory}/install.env" "${invalid_profile_directory}/app"
printf 'INSTALL_PROFILE="invalid"\n' >> "${invalid_profile_directory}/install.env"

if (cd "$invalid_profile_directory" && bash "${PROJECT_ROOT}/install_nexus.sh" --check-config >/dev/null 2>&1); then
  fail 'unknown install profile passed configuration validation'
fi

hosted_directory="${temporary_directory}/hosted"
mkdir -p "$hosted_directory"
prepare_config "${hosted_directory}/install.env" "${hosted_directory}/app"
printf 'NEXUS_RUNTIME="hosted-tenant"\nNEXUS_MANAGED="true"\n' >> "${hosted_directory}/install.env"

if (cd "$hosted_directory" && bash "${PROJECT_ROOT}/install_nexus.sh" --check-config >/dev/null 2>&1); then
  fail 'hosted install.env passed the standalone installer check'
fi

existing_hosted_directory="${temporary_directory}/existing-hosted"
mkdir -p "${existing_hosted_directory}/app"
prepare_config "${existing_hosted_directory}/install.env" "${existing_hosted_directory}/app"
printf 'NEXUS_RUNTIME=hosted-tenant\nNEXUS_MANAGED=true\n' > "${existing_hosted_directory}/app/.env"

if (cd "$existing_hosted_directory" && bash "${PROJECT_ROOT}/install_nexus.sh" --check-config >/dev/null 2>&1); then
  fail 'an existing hosted AMS environment was converted to standalone'
fi

grep -Fq 'readonly INSTALLER_PHP_VERSION="8.3"' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'installer PHP version is not pinned to 8.3'
grep -Fq 'readonly INSTALLER_NODE_MAJOR="22"' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'installer Node major is not pinned to 22'
! grep -Fq 'php8.5' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'unsupported PHP 8.5 package remains in the installer'
grep -Fq 'set_env_kv "NEXUS_RUNTIME" "standalone"' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'standalone runtime is not written to the AMS environment'
grep -Fq 'set_env_kv "NEXUS_MANAGED" "false"' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'managed mode is not disabled in the AMS environment'
grep -Fq 'artisan sync:nations' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'standalone nation synchronization was removed'
grep -Fq 'artisan schedule:run' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'standalone scheduler was removed'
grep -Fq 'SUBS_DELIVERY_DRIVER:=http' "${PROJECT_ROOT}/installer_helpers.sh" \
  || fail 'legacy Subs HTTP delivery is not the default'
grep -Fq 'redis-stream' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'Subs Redis Stream delivery was removed'
grep -Fq 'if $INSTALL_PHP || $INSTALL_DB || $INSTALL_NGINX; then' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'db-only profile cannot enter the database installation stage'
grep -Fq 'if $INSTALL_APP; then' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'Composer is not gated to profiles that install AMS'
grep -Fq 'stdout_logfile=${SUBS_PATH}/logs/subs.log' "${PROJECT_ROOT}/install_nexus.sh" \
  || fail 'subs-only profile still logs into the absent AMS tree'

sensitive_keys=(
  DB_PASSWORD
  NEXUS_API_TOKEN
  PW_API_KEY
  PW_API_MUTATION_KEY
  PW_API_TOKEN
  REDIS_PASSWORD
  SUBS_REDIS_URL
)

for key in "${sensitive_keys[@]}"; do
  calls="$(grep -F "set_env_kv \"${key}\"" "${PROJECT_ROOT}/install_nexus.sh" || true)"
  [[ -n "$calls" ]] || fail "${key} has no installer environment write"

  while IFS= read -r call; do
    [[ "$call" == *'"secret"'* ]] || fail "${key} is written without dry-run redaction"
  done <<< "$calls"
done

if grep -Eqi 'console\.nexusams\.cloud|https://[^[:space:]]*nexusams\.cloud' \
  "${PROJECT_ROOT}/install_nexus.sh" "${PROJECT_ROOT}/install.env"; then
  fail 'standalone installer contains a hosted Cloud dependency'
fi

if grep -Eq 'set_env_kv "(NEXUS_CONTROL_CALLBACK|NEXUS_BOOTSTRAP_INTROSPECTION)' \
  "${PROJECT_ROOT}/install_nexus.sh"; then
  fail 'standalone installer writes hosted-only Cloud configuration'
fi

printf 'ok - legacy settings default safely to standalone\n'
printf 'ok - hosted and world-writer modes fail closed\n'
printf 'ok - existing managed installations are not converted\n'
printf 'ok - credential writes are redacted and exact\n'
printf 'ok - PHP, Node, schedules, sync, and Subs contracts remain explicit\n'
