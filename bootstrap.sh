#!/usr/bin/env bash
set -Eeuo pipefail

if [[ ${EUID} -ne 0 ]]; then
  echo "Run this bootstrap with sudo: sudo ./bootstrap.sh" >&2
  exit 1
fi

if [[ ! -r /etc/os-release ]]; then
  echo "Nexus supports Ubuntu and Debian systemd hosts only." >&2
  exit 1
fi

# shellcheck disable=SC1091
source /etc/os-release
case "${ID:-}:${VERSION_ID:-}" in
  ubuntu:22.04|ubuntu:24.04|ubuntu:26.04|debian:12|debian:13) ;;
  *)
    echo "Unsupported platform: ${ID:-unknown} ${VERSION_ID:-unknown}" >&2
    exit 1
    ;;
esac

if [[ $(dpkg --print-architecture) != amd64 ]]; then
  echo "Nexus currently supports amd64 hosts only." >&2
  exit 1
fi

if [[ ! -d /run/systemd/system ]]; then
  echo "Nexus requires a host booted with systemd." >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl
fi

temporary_directory=$(mktemp -d /var/tmp/nexus-bootstrap.XXXXXXXX)
trap 'rm -rf -- "$temporary_directory"' EXIT

asset_url='https://github.com/Yosodog/Nexus-Setup/releases/latest/download/nexus-linux-amd64'
curl --fail --location --proto '=https' --tlsv1.2 \
  --output "${temporary_directory}/nexus" \
  "$asset_url"

install -o root -g root -m 0755 "${temporary_directory}/nexus" /usr/local/bin/nexus
/usr/local/bin/nexus internal configure-host

echo
echo "The Nexus command is ready. Continue with:"
echo "  sudo nexus install"
