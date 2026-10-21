#!/usr/bin/env bash
set -Eeuo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
bootstrap="${project_root}/bootstrap.sh"

bash -n "${bootstrap}" "${project_root}/install_nexus.sh"
grep -Fq 'releases/download/${tag}/${asset}' "${bootstrap}"
grep -Fq 'sha256sum --check --status --strict' "${bootstrap}"
grep -Fq '/usr/local/bin/nexus internal configure-host' "${bootstrap}"
grep -Fq 'sudo nexus install --channel beta' "${bootstrap}"
grep -Fq '[[ ! -d /run/systemd/system ]]' "${bootstrap}"
grep -Fq -e "--proto '=https'" "${bootstrap}"

if grep -v '^[[:space:]]*#' "${bootstrap}" | grep -Eq 'curl[^|]*\|[[:space:]]*(sudo[[:space:]]+)?(ba)?sh|eval[[:space:]]'; then
  echo 'bootstrap contains an unsafe shell execution pattern' >&2
  exit 1
fi

# shellcheck source=../bootstrap.sh
NEXUS_BOOTSTRAP_LIBRARY=1 source "${bootstrap}"
set +e

listing='[
  {"url": "x", "tag_name": "v1.3.0-beta.2", "prerelease": true},
  {"url": "y", "tag_name": "v1.2.0", "prerelease": false}
]'
[[ $(newest_release_tag <<<"${listing}") == v1.3.0-beta.2 ]] || { echo 'beta tag was not parsed from the releases listing' >&2; exit 1; }
[[ $(resolve_tag stable v2.0.0-rc.1) == v2.0.0-rc.1 ]] || { echo 'an explicit version was not used' >&2; exit 1; }
if resolve_tag stable 'v1.0.0;id' 2>/dev/null; then
  echo 'an unsafe version was accepted' >&2
  exit 1
fi
if main --channel nightly 2>/dev/null; then
  echo 'an unknown channel was accepted' >&2
  exit 1
fi

# The EXIT trap runs after main has returned, under set -u.
work_directory=$(mktemp -d)
cleanup_output=$(bash -c '
  set -Eeuo pipefail
  NEXUS_BOOTSTRAP_LIBRARY=1 source "$1"
  run() {
    trap cleanup EXIT
    download_directory=$(mktemp -d "$2/nexus-bootstrap.XXXXXXXX")
  }
  run "$@"
' bootstrap "${bootstrap}" "${work_directory}" 2>&1)
leftover=$(find "${work_directory}" -mindepth 1 -print -quit)
rmdir "${work_directory}" 2>/dev/null
if [[ -n ${cleanup_output} || -n ${leftover} ]]; then
  echo "bootstrap did not clean up its download directory: ${cleanup_output}" >&2
  exit 1
fi

echo 'ok - bootstrap installs a checksum-verified official Nexus CLI release'
echo 'ok - bootstrap resolves stable, beta, and pinned releases'
echo 'ok - bootstrap removes its download directory on exit'
echo 'ok - legacy installer is retired'
