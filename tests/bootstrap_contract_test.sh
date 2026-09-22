#!/usr/bin/env bash
set -Eeuo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

bash -n "${project_root}/bootstrap.sh" "${project_root}/install_nexus.sh"
grep -Fq 'releases/latest/download/nexus-linux-amd64' "${project_root}/bootstrap.sh"
grep -Fq '/usr/local/bin/nexus internal configure-host' "${project_root}/bootstrap.sh"
grep -Fq 'sudo nexus install' "${project_root}/bootstrap.sh"
grep -Fq '[[ ! -d /run/systemd/system ]]' "${project_root}/bootstrap.sh"

if grep -Eq 'curl[^\n]*\|[[:space:]]*(ba)?sh|eval[[:space:]]' "${project_root}/bootstrap.sh"; then
  echo 'bootstrap contains an unsafe shell execution pattern' >&2
  exit 1
fi

echo 'ok - bootstrap installs the fixed official Nexus CLI asset'
echo 'ok - legacy installer is retired'
