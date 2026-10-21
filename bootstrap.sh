#!/usr/bin/env bash
# Installs the nexus command. Run it from a checkout or straight from GitHub:
#
#   curl -fsSL https://raw.githubusercontent.com/Yosodog/Nexus-Setup/main/bootstrap.sh | sudo bash
#   curl -fsSL https://raw.githubusercontent.com/Yosodog/Nexus-Setup/main/bootstrap.sh | sudo bash -s -- --channel beta
#
# Everything runs inside main, so a partially downloaded script does nothing.
set -Eeuo pipefail

readonly repository_url='https://github.com/Yosodog/Nexus-Setup'
readonly releases_api='https://api.github.com/repos/Yosodog/Nexus-Setup/releases?per_page=30'
readonly asset='nexus-linux-amd64'
readonly tag_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$'

# The download directory is global so the EXIT trap can still see it after
# main returns; a local would be out of scope by then.
download_directory=''

cleanup() {
  if [[ -n ${download_directory} ]]; then
    rm -rf -- "${download_directory}"
  fi
}

usage() {
  echo 'usage: bootstrap.sh [--channel stable|beta] [--version vX.Y.Z]' >&2
}

fetch() {
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "$@"
}

# latest_stable_tag reads the tag GitHub redirects /releases/latest to.
latest_stable_tag() {
  local effective_url
  effective_url=$(fetch --head --output /dev/null --write-out '%{url_effective}' "${repository_url}/releases/latest")
  printf '%s\n' "${effective_url##*/}"
}

# newest_release_tag prints the first tag in a GitHub releases listing, which
# is the most recently created published release, prerelease or not.
newest_release_tag() {
  grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/'
}

resolve_tag() {
  local channel=$1 version=$2 tag
  if [[ -n ${version} ]]; then
    tag=${version}
  elif [[ ${channel} == beta ]]; then
    tag=$(fetch --header 'Accept: application/vnd.github+json' "${releases_api}" | newest_release_tag)
  else
    tag=$(latest_stable_tag)
  fi
  if [[ ! ${tag} =~ ${tag_pattern} ]]; then
    echo "No published Nexus CLI release was found for the ${channel} channel." >&2
    echo 'Check access to github.com and api.github.com, then retry.' >&2
    return 1
  fi
  printf '%s\n' "${tag}"
}

check_platform() {
  if [[ ! -r /etc/os-release ]]; then
    echo 'Nexus supports Ubuntu and Debian systemd hosts only.' >&2
    return 1
  fi
  # shellcheck disable=SC1091
  source /etc/os-release
  case "${ID:-}:${VERSION_ID:-}" in
    ubuntu:22.04|ubuntu:24.04|ubuntu:26.04|debian:12|debian:13) ;;
    *)
      echo "Unsupported platform: ${ID:-unknown} ${VERSION_ID:-unknown}" >&2
      return 1
      ;;
  esac
  if [[ $(dpkg --print-architecture) != amd64 ]]; then
    echo 'Nexus currently supports amd64 hosts only.' >&2
    return 1
  fi
  if [[ ! -d /run/systemd/system ]]; then
    echo 'Nexus requires a host booted with systemd.' >&2
    return 1
  fi
}

main() {
  local channel=stable version=''
  while [[ $# -gt 0 ]]; do
    case $1 in
      --channel)
        channel=${2:-}
        shift 2 || { usage; return 1; }
        ;;
      --version)
        version=${2:-}
        shift 2 || { usage; return 1; }
        ;;
      -h|--help)
        usage
        return 0
        ;;
      *)
        usage
        return 1
        ;;
    esac
  done
  if [[ ${channel} != stable && ${channel} != beta ]]; then
    usage
    return 1
  fi

  if [[ ${EUID} -ne 0 ]]; then
    echo 'Run this bootstrap with sudo.' >&2
    return 1
  fi
  check_platform

  if ! command -v curl >/dev/null 2>&1; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl
  fi

  local tag
  tag=$(resolve_tag "${channel}" "${version}")
  trap cleanup EXIT
  download_directory=$(mktemp -d /var/tmp/nexus-bootstrap.XXXXXXXX)

  echo "Downloading the Nexus CLI ${tag}..."
  fetch --output "${download_directory}/${asset}" "${repository_url}/releases/download/${tag}/${asset}"
  fetch --output "${download_directory}/${asset}.sha256" "${repository_url}/releases/download/${tag}/${asset}.sha256"
  if ! (cd "${download_directory}" && sha256sum --check --status --strict "${asset}.sha256"); then
    echo "The downloaded Nexus CLI does not match its published SHA-256 checksum." >&2
    return 1
  fi

  install -o root -g root -m 0755 "${download_directory}/${asset}" /usr/local/bin/nexus
  /usr/local/bin/nexus internal configure-host

  echo
  echo "The Nexus command ${tag} is ready. Continue with:"
  if [[ ${channel} == beta ]]; then
    echo '  sudo nexus install --channel beta'
  else
    echo '  sudo nexus install'
  fi
}

if [[ ${NEXUS_BOOTSTRAP_LIBRARY:-} != 1 ]]; then
  main "$@"
fi
