#!/usr/bin/env bash
# Nexus AMS installer (multi-distro)
# Version: 2.1.0

set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

# ---------------------------------------------------------------------------
# CLI FLAGS
# ---------------------------------------------------------------------------
DRY_RUN=false
FORCE_NON_INTERACTIVE=false

for arg in "${@:-}"; do
  case "$arg" in
    --dry-run)         DRY_RUN=true ;;
    --non-interactive) FORCE_NON_INTERACTIVE=true ;;
  esac
done

# ---------------------------------------------------------------------------
# LOGGING
# ---------------------------------------------------------------------------
LOG_FILE="/var/log/nexus-install.log"
mkdir -p "$(dirname "$LOG_FILE")"
exec > >(tee -a "$LOG_FILE") 2>&1

log()  { printf "\n\033[1;32m==> %s\033[0m\n" "$*"; }
warn() { printf "\n\033[1;33m[!] %s\033[0m\n" "$*"; }
err()  { printf "\n\033[1;31m[ERROR] %s\033[0m\n" "$*"; }

run() {
  if $DRY_RUN; then
    echo "[dry-run] $*"
  else
    eval "$@"
  fi
}

die() { err "$1"; exit 1; }

require_root() { [[ $EUID -eq 0 ]] || die "Run as root (sudo)."; }

# ---------------------------------------------------------------------------
# OS DETECTION & PKG HELPERS (Ubuntu/Debian + RHEL/Amazon Linux)
# ---------------------------------------------------------------------------
detect_os() {
  [[ -f /etc/os-release ]] || die "/etc/os-release not found. Unsupported OS."
  # shellcheck disable=SC1091
  . /etc/os-release

  OS_ID="${ID:-}"
  OS_LIKE="${ID_LIKE:-}"

  IS_DEBIAN=false
  IS_RHEL=false

  if [[ "$OS_ID" == "ubuntu" || "$OS_ID" == "debian" || "$OS_LIKE" == *"debian"* ]]; then
    IS_DEBIAN=true
    PKG_MGR="apt-get"
  elif [[ "$OS_ID" == "amzn" || "$OS_LIKE" == *"rhel"* || "$OS_LIKE" == *"fedora"* || "$OS_LIKE" == *"centos"* ]]; then
    IS_RHEL=true
    if command -v dnf >/dev/null 2>&1; then
      PKG_MGR="dnf"
    elif command -v yum >/dev/null 2>&1; then
      PKG_MGR="yum"
    else
      die "No dnf or yum found on RHEL-like system."
    fi
  else
    die "Unsupported OS: ID=${OS_ID}, ID_LIKE=${OS_LIKE}. Supported: Ubuntu/Debian, RHEL, Amazon Linux."
  fi
}

pkg_update() {
  if $IS_DEBIAN; then
    run "$PKG_MGR update && $PKG_MGR -y \
      -o Dpkg::Options::=--force-confdef \
      -o Dpkg::Options::=--force-confold upgrade"
  else
    run "$PKG_MGR -y update || true"
  fi
}

pkg_install() {
  if $IS_DEBIAN; then
    run "$PKG_MGR install -y $*"
  else
    run "$PKG_MGR install -y $*"
  fi
}

# ---------------------------------------------------------------------------
# WEB USER DETECTION
# ---------------------------------------------------------------------------
detect_web_user() {
  if id www-data &>/dev/null; then
    WEB_USER="www-data"
  elif id nginx &>/dev/null; then
    WEB_USER="nginx"
  elif id apache &>/dev/null; then
    WEB_USER="apache"
  else
    WEB_USER="www-data"
    warn "Web user not found; defaulting to www-data. You may need to adjust user/group manually."
  fi
}

# ---------------------------------------------------------------------------
# REDIS CONFIG
# ---------------------------------------------------------------------------
configure_redis_maxmemory() {
  local maxmem="$1"

  local conf_candidates=(
    "/etc/redis/redis.conf"
    "/etc/redis.conf"
  )

  local conf=""
  for c in "${conf_candidates[@]}"; do
    if [[ -f "$c" ]]; then
      conf="$c"
      break
    fi
  done

  if [[ -z "$conf" ]]; then
    warn "Redis config file not found; skipping maxmemory config."
    return
  fi

  log "Configuring Redis maxmemory=${maxmem} in ${conf}"

  if $DRY_RUN; then
    echo "[dry-run] Update maxmemory in $conf"
    return
  fi

  if grep -qE '^[[:space:]]*maxmemory ' "$conf"; then
    sed -i "s/^[[:space:]]*maxmemory .*/maxmemory ${maxmem}/" "$conf"
  else
    echo "maxmemory ${maxmem}" >> "$conf"
  fi

  if ! grep -qE '^[[:space:]]*maxmemory-policy ' "$conf"; then
    echo "maxmemory-policy allkeys-lru" >> "$conf"
  fi

  systemctl restart redis* 2>/dev/null || systemctl restart redis || true
}

# ---------------------------------------------------------------------------
# ASCII BANNER
# ---------------------------------------------------------------------------
print_banner() {
cat <<'BANNER'
==========================================================================

███╗   ██╗███████╗██╗  ██╗██╗   ██╗███████╗     █████╗ ███╗   ███╗███████╗
████╗  ██║██╔════╝╚██╗██╔╝██║   ██║██╔════╝    ██╔══██╗████╗ ████║██╔════╝
██╔██╗ ██║█████╗   ╚███╔╝ ██║   ██║███████╗    ███████║██╔████╔██║███████╗
██║╚██╗██║██╔══╝   ██╔██╗ ██║   ██║╚════██║    ██╔══██║██║╚██╔╝██║╚════██║
██║ ╚████║███████╗██╔╝ ██╗╚██████╔╝███████║    ██║  ██║██║ ╚═╝ ██║███████║
╚═╝  ╚═══╝╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚══════╝    ╚═╝  ╚═╝╚═╝     ╚═╝╚══════╝
                                                                        
                        Nexus AMS Installer v2.0.0
==========================================================================
BANNER
}

# ---------------------------------------------------------------------------
# INSTALL PROFILES (section toggles)
# ---------------------------------------------------------------------------
INSTALL_BASE=true
INSTALL_SWAP=true
INSTALL_PHP=true
INSTALL_NGINX=true
INSTALL_DB=true
INSTALL_APP=true
INSTALL_SUBS=true
CONFIGURE_NGINX=true
CONFIGURE_SUPERVISOR=true
CONFIGURE_CRON=true
RUN_INITIAL_JOBS=true
CREATE_ADMIN_USER_FLAG=true

set_profile_flags() {
  local profile="$1"

  # Reset to full
  INSTALL_BASE=true
  INSTALL_SWAP=true
  INSTALL_PHP=true
  INSTALL_NGINX=true
  INSTALL_DB=true
  INSTALL_APP=true
  INSTALL_SUBS=true
  CONFIGURE_NGINX=true
  CONFIGURE_SUPERVISOR=true
  CONFIGURE_CRON=true
  RUN_INITIAL_JOBS=true
  CREATE_ADMIN_USER_FLAG=true

  case "$profile" in
    full)
      ;; # full stack, no change
    app-web-subs-remote-db)
      INSTALL_DB=false
      ;;
    web-only)
      INSTALL_DB=false
      INSTALL_SUBS=false
      ;;
    db-only)
      INSTALL_PHP=false
      INSTALL_NGINX=false
      INSTALL_APP=false
      INSTALL_SUBS=false
      CONFIGURE_NGINX=false
      CONFIGURE_SUPERVISOR=false
      CONFIGURE_CRON=false
      RUN_INITIAL_JOBS=false
      CREATE_ADMIN_USER_FLAG=false
      ;;
    subs-only)
      INSTALL_PHP=false
      INSTALL_NGINX=false
      INSTALL_DB=false
      INSTALL_APP=false
      CONFIGURE_NGINX=false
      CONFIGURE_CRON=false
      RUN_INITIAL_JOBS=false
      CREATE_ADMIN_USER_FLAG=false
      ;;
    *)
      die "Unknown install profile: $profile"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# INTERACTIVE SETUP (default mode)
# ---------------------------------------------------------------------------
ENV_PATH="./install.env"

interactive_prompt() {
  print_banner

  echo ""
  echo "Interactive setup (this will write/overwrite install.env in the current directory)."
  echo ""

  read -r -p "Domain for Nexus AMS (e.g. nexus.example.com): " DOMAIN
  DOMAIN="${DOMAIN:-nexus.local}"

  read -r -p "App path [/var/www/nexus]: " APP_PATH
  APP_PATH="${APP_PATH:-/var/www/nexus}"

  # Subs default path not under www
  read -r -p "Subs path [/var/nexus-subs]: " SUBS_PATH
  SUBS_PATH="${SUBS_PATH:-/var/nexus-subs}"

  read -r -p "App name [Nexus AMS]: " APP_NAME
  APP_NAME="${APP_NAME:-Nexus AMS}"

  local default_url="https://${DOMAIN}"
  read -r -p "APP_URL [${default_url}]: " APP_URL
  APP_URL="${APP_URL:-$default_url}"

  echo ""
  echo "Install profile:"
  echo "  [1] full                      (App + Web + DB + Subs)"
  echo "  [2] app-web-subs-remote-db    (App + Web + Subs, DB on remote host)"
  echo "  [3] web-only                  (App + Web, DB on remote host, no Subs)"
  echo "  [4] db-only                   (DB server only)"
  echo "  [5] subs-only                 (Subscriptions worker only)"
  read -r -p "Select [1-5]: " profile_choice

  case "$profile_choice" in
    1|"") INSTALL_PROFILE="full" ;;
    2) INSTALL_PROFILE="app-web-subs-remote-db" ;;
    3) INSTALL_PROFILE="web-only" ;;
    4) INSTALL_PROFILE="db-only" ;;
    5) INSTALL_PROFILE="subs-only" ;;
    *) INSTALL_PROFILE="full" ;;
  esac

  # Swap config
  echo ""
  echo "Swap configuration:"
  read -r -p "Enable swap creation? [Y/n]: " enable_swap_ans
  if [[ "${enable_swap_ans,,}" == "n" ]]; then
    ENABLE_SWAP="false"
    SWAP_SIZE_GB=""
  else
    ENABLE_SWAP="true"
    read -r -p "Swap size in GB [4]: " SWAP_SIZE_GB
    SWAP_SIZE_GB="${SWAP_SIZE_GB:-4}"
  fi

  # Database details
  echo ""
  echo "Database configuration (for Laravel app):"
  if [[ "$INSTALL_PROFILE" == "full" || "$INSTALL_PROFILE" == "db-only" ]]; then
    DB_HOST="127.0.0.1"
    echo "DB host set to 127.0.0.1 for profile $INSTALL_PROFILE"
  else
    read -r -p "DB host [127.0.0.1]: " DB_HOST
    DB_HOST="${DB_HOST:-127.0.0.1}"
  fi

  read -r -p "DB name [nexus]: " DB_DATABASE
  DB_DATABASE="${DB_DATABASE:-nexus}"

  read -r -p "DB username [nexus]: " DB_USERNAME
  DB_USERNAME="${DB_USERNAME:-nexus}"

  read -r -p "DB password [nexus-pass]: " DB_PASSWORD
  DB_PASSWORD="${DB_PASSWORD:-nexus-pass}"

  # Redis choice
  echo ""
  echo "Cache / Queue / Session configuration:"
  echo "  [1] Database / file (simpler, no Redis)"
  echo "  [2] Redis (recommended)"
  read -r -p "Select [1-2]: " redis_choice

  if [[ "$redis_choice" == "2" ]]; then
    USE_REDIS="true"
    read -r -p "Redis max memory (e.g. 256mb) [256mb]: " REDIS_MAX_MEMORY
    REDIS_MAX_MEMORY="${REDIS_MAX_MEMORY:-256mb}"
  else
    USE_REDIS="false"
    REDIS_MAX_MEMORY=""
  fi

  # Nexus / PW config
  echo ""
  echo "Nexus / PW configuration (you can leave blank and edit later in .env):"
  local default_nexus_api_url="https://example.com/api/v1/subs"
  read -r -p "NEXUS_API_URL [${default_nexus_api_url}]: " NEXUS_API_URL
  NEXUS_API_URL="${NEXUS_API_URL:-$default_nexus_api_url}"

  read -r -p "NEXUS_API_TOKEN: " NEXUS_API_TOKEN
  read -r -p "PW_API_KEY: " PW_API_KEY
  read -r -p "PW_API_MUTATION_KEY: " PW_API_MUTATION_KEY
  read -r -p "PW_ALLIANCE_ID: " PW_ALLIANCE_ID

  read -r -p "Enable snapshots in Subs? [y/N]: " enable_snapshots_ans
  if [[ "${enable_snapshots_ans,,}" == "y" ]]; then
    ENABLE_SNAPSHOTS="true"
  else
    ENABLE_SNAPSHOTS="false"
  fi

  # Admin user setup
  echo ""
  read -r -p "Create initial admin user now? [Y/n]: " create_admin_ans
  if [[ "${create_admin_ans,,}" == "n" ]]; then
    CREATE_ADMIN_USER="false"
    ADMIN_NAME=""
    ADMIN_EMAIL=""
    ADMIN_PASSWORD=""
    ADMIN_NATION_ID="0"
    ADMIN_ROLE_ID="1"
  else
    CREATE_ADMIN_USER="true"
    read -r -p "Admin name [Nexus Admin]: " ADMIN_NAME
    ADMIN_NAME="${ADMIN_NAME:-Nexus Admin}"

    local default_admin_email="admin@${DOMAIN}"
    read -r -p "Admin email [${default_admin_email}]: " ADMIN_EMAIL
    ADMIN_EMAIL="${ADMIN_EMAIL:-$default_admin_email}"

    read -r -p "Admin password [change-me]: " ADMIN_PASSWORD
    ADMIN_PASSWORD="${ADMIN_PASSWORD:-change-me}"
    read -r -p "Admin nation ID (numeric, default 0): " ADMIN_NATION_ID
    ADMIN_NATION_ID="${ADMIN_NATION_ID:-0}"
    read -r -p "Admin role_id (numeric, default 1): " ADMIN_ROLE_ID
    ADMIN_ROLE_ID="${ADMIN_ROLE_ID:-1}"
  fi

  # Certbot contact (default placeholder)
  echo ""
  local default_certbot_email="yourname@example.com"
  read -r -p "Certbot admin email [${default_certbot_email}]: " CERTBOT_EMAIL
  CERTBOT_EMAIL="${CERTBOT_EMAIL:-$default_certbot_email}"

  # Show summary before writing env / running
  echo ""
  echo "======== CONFIG SUMMARY ========"
  echo "Domain:            $DOMAIN"
  echo "App path:          $APP_PATH"
  echo "Subs path:         $SUBS_PATH"
  echo "Profile:           $INSTALL_PROFILE"
  echo "Enable swap:       ${ENABLE_SWAP:-true}"
  echo "Swap size (GB):    ${SWAP_SIZE_GB:-4}"
  echo "DB host:           $DB_HOST"
  echo "DB name:           $DB_DATABASE"
  echo "DB user:           $DB_USERNAME"
  echo "Use Redis:         $USE_REDIS"
  echo "Redis maxmemory:   ${REDIS_MAX_MEMORY:-N/A}"
  echo "NEXUS_API_URL:     $NEXUS_API_URL"
  echo "Enable snapshots:  $ENABLE_SNAPSHOTS"
  echo "Create admin user: $CREATE_ADMIN_USER"
  echo "Admin email:       ${ADMIN_EMAIL:-N/A}"
  echo "Certbot email:     $CERTBOT_EMAIL"
  echo "================================"
  echo ""

  read -r -p "Proceed with installation using these settings? [y/N]: " confirm_run
  if [[ "${confirm_run,,}" != "y" ]]; then
    die "Installation aborted by user."
  fi

  # Write install.env for future runs
  if ! $DRY_RUN; then
    log "Writing ${ENV_PATH} for future runs..."
    cat > "$ENV_PATH" <<EOF
DOMAIN="$DOMAIN"
APP_PATH="$APP_PATH"
SUBS_PATH="$SUBS_PATH"
APP_NAME="$APP_NAME"
APP_URL="$APP_URL"

INSTALL_PROFILE="$INSTALL_PROFILE"

ENABLE_SWAP="$ENABLE_SWAP"
SWAP_SIZE_GB="$SWAP_SIZE_GB"

DB_HOST="$DB_HOST"
DB_DATABASE="$DB_DATABASE"
DB_USERNAME="$DB_USERNAME"
DB_PASSWORD="$DB_PASSWORD"

USE_REDIS="$USE_REDIS"
REDIS_MAX_MEMORY="$REDIS_MAX_MEMORY"

NEXUS_API_URL="$NEXUS_API_URL"
NEXUS_API_TOKEN="$NEXUS_API_TOKEN"
PW_API_KEY="$PW_API_KEY"
PW_API_MUTATION_KEY="$PW_API_MUTATION_KEY"
PW_ALLIANCE_ID="$PW_ALLIANCE_ID"
ENABLE_SNAPSHOTS="$ENABLE_SNAPSHOTS"

CREATE_ADMIN_USER="$CREATE_ADMIN_USER"
ADMIN_NAME="$ADMIN_NAME"
ADMIN_EMAIL="$ADMIN_EMAIL"
ADMIN_PASSWORD="$ADMIN_PASSWORD"
ADMIN_NATION_ID="$ADMIN_NATION_ID"
ADMIN_ROLE_ID="$ADMIN_ROLE_ID"
CERTBOT_EMAIL="$CERTBOT_EMAIL"
EOF
  fi
}

# ---------------------------------------------------------------------------
# ENV LOADING (interactive default, --non-interactive uses existing env)
# ---------------------------------------------------------------------------
require_root
detect_os
detect_web_user

log "Nexus AMS Installer v2.1.0"
$DRY_RUN && warn "Dry-run mode enabled. Commands will be printed but not executed."

if $FORCE_NON_INTERACTIVE; then
  [[ -f "$ENV_PATH" ]] || die "install.env not found. It is required for --non-interactive mode."
  log "Non-interactive mode: using existing install.env"
  # shellcheck disable=SC1090
  source "$ENV_PATH"
else
  # Interactive is default
  interactive_prompt
  # shellcheck disable=SC1090
  source "$ENV_PATH"
fi

# Defaults for new fields if missing (for non-interactive legacy envs)
: "${ENABLE_SWAP:=true}"
: "${SWAP_SIZE_GB:=4}"
: "${CERTBOT_EMAIL:=yourname@example.com}"

set_profile_flags "${INSTALL_PROFILE:-full}"

PHP_FPM_SOCK=""
NGINX_SITE=""
NGINX_ENABLED=""

# ---------------------------------------------------------------------------
# PORT CHECKS
# ---------------------------------------------------------------------------
log "Checking if ports 80/443 are already in use"
if ss -tulpn | grep -q ":80 "; then warn "Port 80 appears in use. Continuing (Nginx will likely reuse it)."; fi
if ss -tulpn | grep -q ":443 "; then warn "Port 443 appears in use. Continuing (Nginx/Certbot will handle)."; fi

# ---------------------------------------------------------------------------
# STAGE FUNCTIONS
# ---------------------------------------------------------------------------

stage_base() {
  log "Stage 1: System update & base packages"
  pkg_update
  if $IS_DEBIAN; then
    pkg_install "software-properties-common curl git unzip lsb-release ca-certificates apt-transport-https gnupg2"
  else
    pkg_install "curl git unzip ca-certificates"
  fi
}

stage_swap() {
  if [[ "${ENABLE_SWAP,,}" != "true" ]]; then
    log "Stage 2: Swap creation disabled (ENABLE_SWAP=false)"
    return
  fi

  log "Stage 2: Swap (${SWAP_SIZE_GB}G, idempotent)"

  if ! swapon --show | grep -q "/swapfile"; then
    local size_gb="${SWAP_SIZE_GB:-4}"
    local size_str="${size_gb}G"
    run "fallocate -l ${size_str} /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=$((size_gb * 1024)) status=progress"
    run "chmod 600 /swapfile"
    run "mkswap /swapfile"
    run "swapon /swapfile"
    grep -q "/swapfile" /etc/fstab || run "echo '/swapfile none swap sw 0 0' >> /etc/fstab"
  else
    log "Swap already present — skipping"
  fi
}

detect_php_fpm_socket() {
  if $IS_DEBIAN; then
    local ver
    ver="$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || echo "8.4")"
    PHP_FPM_SOCK="/run/php/php${ver}-fpm.sock"
  else
    PHP_FPM_SOCK="/run/php-fpm/www.sock"
  fi
}

stage_php_web_stack() {
  log "Stage 3: PHP, MySQL (if enabled), Nginx"

  # PHP
  if $IS_DEBIAN; then
    if ! ls /etc/apt/sources.list.d/ 2>/dev/null | grep -q "ondrej-ubuntu-php"; then
      run "add-apt-repository ppa:ondrej/php -y"
      run "apt-get update"
    fi
    pkg_install "php8.4 php8.4-cli php8.4-fpm php8.4-mysql php8.4-xml php8.4-curl php8.4-mbstring php8.4-zip php8.4-bcmath"
    pkg_install "php-redis" || true
  else
    pkg_install "php php-cli php-fpm php-mysqlnd php-xml php-mbstring php-json php-gd php-bcmath"
    pkg_install "php-pecl-redis" || true
  fi

  detect_php_fpm_socket

  # MySQL (only if INSTALL_DB)
  if $INSTALL_DB; then
    if $IS_DEBIAN; then
      pkg_install "mysql-server"
      run "systemctl enable --now mysql"
    else
      pkg_install "mariadb-server"
      run "systemctl enable --now mariadb || systemctl enable --now mysqld || true"
    fi
  fi

  # Nginx
  if $INSTALL_NGINX; then
    pkg_install "nginx"
    run "systemctl enable --now nginx"
  fi
}

stage_clone_apps() {
  if ! $INSTALL_APP && ! $INSTALL_SUBS; then
    return
  fi

  log "Stage 4: Clone Nexus AMS and Subs"

  if $INSTALL_APP; then
    run "mkdir -p $(dirname "$APP_PATH")"
    if [[ ! -d "$APP_PATH/.git" ]]; then
      run "cd $(dirname "$APP_PATH") && git clone https://github.com/Yosodog/Nexus-AMS.git"
      if [[ "$APP_PATH" != "$(dirname "$APP_PATH")/Nexus-AMS" ]]; then
        run "mv $(dirname "$APP_PATH")/Nexus-AMS $APP_PATH"
      fi
    fi
  fi

  if $INSTALL_SUBS; then
    run "mkdir -p $(dirname "$SUBS_PATH")"
    if [[ ! -d "$SUBS_PATH/.git" ]]; then
      run "cd $(dirname "$SUBS_PATH") && git clone https://github.com/Yosodog/Nexus-AMS-Subs.git"
      if [[ "$SUBS_PATH" != "$(dirname "$SUBS_PATH")/Nexus-AMS-Subs" ]]; then
        run "mv $(dirname "$SUBS_PATH")/Nexus-AMS-Subs $SUBS_PATH"
      fi
    fi
  fi
}

stage_node_composer() {
  if ! $INSTALL_APP && ! $INSTALL_SUBS; then
    log "Stage 5: Node & Composer skipped (no app/subs install)"
    return
  fi

  log "Stage 5: Node LTS & Composer"

  if $IS_DEBIAN; then
    run "curl -fsSL https://deb.nodesource.com/setup_lts.x | bash -"
    pkg_install "nodejs"
  else
    pkg_install "nodejs npm" || true
  fi

  run "php -r \"copy('https://getcomposer.org/installer','composer-setup.php');\""
  run "php composer-setup.php --install-dir=/usr/local/bin --filename=composer"
  run "php -r \"unlink('composer-setup.php');\""
}

stage_database_setup() {
  if ! $INSTALL_DB; then
    log "Stage 6: DB install skipped (INSTALL_DB=false)"
    return
  fi

  log "Stage 6: Create MySQL/MariaDB DB & user (idempotent)"

  local MYSQL_CMD="mysql -uroot"
  if ! $DRY_RUN; then
    if ! $MYSQL_CMD -e "SELECT 1" &>/dev/null; then
      warn "Root socket login failed; trying sudo mysql"
      MYSQL_CMD="sudo mysql"
    fi
  fi

  if $DRY_RUN; then
    echo "[dry-run] Would create database '${DB_DATABASE}' and user '${DB_USERNAME}' with privileges."
  else
    $MYSQL_CMD <<SQL
CREATE DATABASE IF NOT EXISTS ${DB_DATABASE} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS '${DB_USERNAME}'@'%' IDENTIFIED BY '${DB_PASSWORD}';
GRANT ALL PRIVILEGES ON ${DB_DATABASE}.* TO '${DB_USERNAME}'@'%';
FLUSH PRIVILEGES;
SQL
  fi
}

set_env_kv() {
  local key="$1" val="$2"
  local env_file="$3"

  if $DRY_RUN; then
    echo "[dry-run] set $key=$val in $env_file"
    return
  fi

  if grep -q "^$key=" "$env_file" 2>/dev/null; then
    sed -i "s|^$key=.*|$key=${val//|/\\|}|" "$env_file"
  else
    echo "$key=$val" >> "$env_file"
  fi
}

stage_laravel_backend() {
  if ! $INSTALL_APP; then
    log "Stage 7: Laravel backend skipped (INSTALL_APP=false)"
    return
  fi

  log "Stage 7: Configure Laravel .env and install backend"

  run "cd $APP_PATH"
  if [[ ! -f "$APP_PATH/.env" ]]; then run "cp .env.example .env"; fi

  local ENV_FILE="$APP_PATH/.env"
  set_env_kv "APP_NAME" "\"$APP_NAME\"" "$ENV_FILE"
  set_env_kv "APP_ENV" "production" "$ENV_FILE"
  set_env_kv "APP_DEBUG" "false" "$ENV_FILE"
  set_env_kv "APP_URL" "$APP_URL" "$ENV_FILE"

  set_env_kv "DB_CONNECTION" "mysql" "$ENV_FILE"
  set_env_kv "DB_HOST" "$DB_HOST" "$ENV_FILE"
  set_env_kv "DB_PORT" "3306" "$ENV_FILE"
  set_env_kv "DB_DATABASE" "$DB_DATABASE" "$ENV_FILE"
  set_env_kv "DB_USERNAME" "$DB_USERNAME" "$ENV_FILE"
  set_env_kv "DB_PASSWORD" "$DB_PASSWORD" "$ENV_FILE"

  # Redis or DB/file
  if [[ "${USE_REDIS,,}" == "true" ]]; then
    set_env_kv "CACHE_DRIVER" "redis" "$ENV_FILE"
    set_env_kv "QUEUE_CONNECTION" "redis" "$ENV_FILE"
    set_env_kv "SESSION_DRIVER" "redis" "$ENV_FILE"
    set_env_kv "REDIS_CLIENT" "phpredis" "$ENV_FILE"
    set_env_kv "REDIS_HOST" "127.0.0.1" "$ENV_FILE"
    set_env_kv "REDIS_PORT" "6379" "$ENV_FILE"
    set_env_kv "REDIS_PASSWORD" "null" "$ENV_FILE"
  else
    set_env_kv "CACHE_DRIVER" "file" "$ENV_FILE"
    set_env_kv "QUEUE_CONNECTION" "database" "$ENV_FILE"
    set_env_kv "SESSION_DRIVER" "file" "$ENV_FILE"
  fi

  set_env_kv "PW_API_KEY" "$PW_API_KEY" "$ENV_FILE"
  set_env_kv "PW_API_MUTATION_KEY" "$PW_API_MUTATION_KEY" "$ENV_FILE"
  set_env_kv "NEXUS_API_TOKEN" "$NEXUS_API_TOKEN" "$ENV_FILE"
  set_env_kv "PW_ALLIANCE_ID" "$PW_ALLIANCE_ID" "$ENV_FILE"

  run "chmod 600 $APP_PATH/.env"

  run "cd $APP_PATH && composer install --no-dev --optimize-autoloader"
  run "cd $APP_PATH && php artisan key:generate --force"
  run "cd $APP_PATH && php artisan migrate --force"
  run "cd $APP_PATH && php artisan db:seed --force"
}

stage_frontend_build() {
  if ! $INSTALL_APP; then
    log "Stage 8: Frontend build skipped (INSTALL_APP=false)"
    return
  fi

  log "Stage 8: Frontend deps & Vite build"
  run "cd $APP_PATH && npm ci"
  if ! $DRY_RUN; then
    find "$APP_PATH/node_modules" -path "*/@esbuild/*/bin/esbuild" -type f -exec chmod +x {} \; 2>/dev/null || true
  fi
  run "cd $APP_PATH && npm run build"
  run "chown -R $WEB_USER:$WEB_USER $APP_PATH/public/build"
}

stage_subs_install() {
  if ! $INSTALL_SUBS; then
    log "Stage 9: Subs install skipped (INSTALL_SUBS=false)"
    return
  fi

  log "Stage 9: Configure Subs .env & install"
  run "cd $SUBS_PATH"
  if [[ ! -f "$SUBS_PATH/.env" ]]; then run "cp .env.example .env"; fi

  local ENV_FILE="$SUBS_PATH/.env"
  set_env_kv "PW_API_TOKEN" "$PW_API_KEY" "$ENV_FILE"
  set_env_kv "NEXUS_API_URL" "$NEXUS_API_URL" "$ENV_FILE"
  set_env_kv "NEXUS_API_TOKEN" "$NEXUS_API_TOKEN" "$ENV_FILE"
  set_env_kv "ENABLE_SNAPSHOTS" "$ENABLE_SNAPSHOTS" "$ENV_FILE"

  run "cd $SUBS_PATH && npm ci"
  run "chown -R $WEB_USER:$WEB_USER $SUBS_PATH"
}

stage_nginx_tls() {
  if ! $CONFIGURE_NGINX; then
    log "Stage 10: Nginx/TLS skipped (CONFIGURE_NGINX=false)"
    return
  fi
  if ! $INSTALL_NGINX; then
    warn "Nginx is not installed but CONFIGURE_NGINX=true; skipping vhost."
    return
  fi

  log "Stage 10: Nginx vhost for $DOMAIN + Certbot"

  if $IS_DEBIAN; then
    NGINX_SITE="/etc/nginx/sites-available/nexus.conf"
    NGINX_ENABLED="/etc/nginx/sites-enabled/nexus.conf"
  else
    NGINX_SITE="/etc/nginx/conf.d/nexus.conf"
    NGINX_ENABLED="$NGINX_SITE"
  fi

  local SITE_BLOCK="
server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name ${DOMAIN};

    root ${APP_PATH}/public;
    index index.php index.html index.htm;

    ssl_certificate /etc/letsencrypt/live/${DOMAIN}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${DOMAIN}/privkey.pem;
    include /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem;

    location / { try_files \$uri \$uri/ /index.php?\$query_string; }

    location ~ \.php$ {
        include fastcgi_params;
        fastcgi_pass unix:${PHP_FPM_SOCK};
        fastcgi_param SCRIPT_FILENAME \$realpath_root\$fastcgi_script_name;
        include fastcgi.conf;
    }

    location ~ /\. { deny all; }

    client_max_body_size 64M;
    gzip on;
    gzip_types text/plain text/css application/json application/javascript text/xml application/xml application/xml+rss text/javascript;
    gzip_vary on;
}
server {
    listen 80;
    listen [::]:80;
    server_name ${DOMAIN};
    return 301 https://\$host\$request_uri;
}
"
  if $DRY_RUN; then
    echo "[dry-run] Write Nginx site to $NGINX_SITE"
  else
    printf "%s\n" "$SITE_BLOCK" > "$NGINX_SITE"
  fi

  if $IS_DEBIAN; then
    if [[ ! -L "$NGINX_ENABLED" ]]; then
      run "ln -sf $NGINX_SITE $NGINX_ENABLED"
    fi
  fi

  run "nginx -t"
  run "systemctl reload nginx"

  # Certbot
  if $IS_DEBIAN; then
    pkg_install "certbot python3-certbot-nginx"
  else
    pkg_install "certbot python3-certbot-nginx || true"
  fi

  run "certbot --nginx -d $DOMAIN --non-interactive --agree-tos -m $CERTBOT_EMAIL --redirect || true"
  run "certbot renew --dry-run || true"
  run "nginx -t && systemctl reload nginx"
}

stage_supervisor() {
  if ! $CONFIGURE_SUPERVISOR; then
    log "Stage 11: Supervisor skipped (CONFIGURE_SUPERVISOR=false)"
    return
  fi

  log "Stage 11: Supervisor processes"
  pkg_install "supervisor"
  run "systemctl enable --now supervisor"

  if $INSTALL_APP; then
    local WORKER_CONF="/etc/supervisor/conf.d/nexus-worker.conf"
    local WORKER_CONTENT="[program:nexus-worker]
process_name=%(program_name)s_%(process_num)02d
directory=${APP_PATH}
command=/usr/bin/php artisan queue:work --queue=default --sleep=3 --tries=3 --max-time=3600
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
numprocs=2
user=${WEB_USER}
redirect_stderr=true
stdout_logfile=${APP_PATH}/storage/logs/worker.log
stopwaitsecs=10
"
    if $DRY_RUN; then echo "[dry-run] Write $WORKER_CONF"; else printf "%s" "$WORKER_CONTENT" > "$WORKER_CONF"; fi

    local WORKER_SYNC_CONF="/etc/supervisor/conf.d/nexus-worker-sync.conf"
    local WORKER_SYNC_CONTENT="[program:nexus-worker-sync]
process_name=%(program_name)s_%(process_num)02d
directory=${APP_PATH}
command=/usr/bin/php artisan queue:work --queue=sync --sleep=3 --tries=3 --max-time=3600
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
numprocs=1
user=${WEB_USER}
redirect_stderr=true
stdout_logfile=${APP_PATH}/storage/logs/worker-sync.log
stopwaitsecs=10
"
    if $DRY_RUN; then echo "[dry-run] Write $WORKER_SYNC_CONF"; else printf "%s" "$WORKER_SYNC_CONTENT" > "$WORKER_SYNC_CONF"; fi
  fi

  if $INSTALL_SUBS; then
    local SUBS_CONF="/etc/supervisor/conf.d/nexus-subs.conf"
    local SUBS_CONTENT="[program:nexus-subs]
directory=${SUBS_PATH}/src
command=/usr/bin/node index.js
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
user=${WEB_USER}
environment=NODE_ENV=\"production\",PATH=\"/usr/bin\"
stdout_logfile=${APP_PATH}/storage/logs/subs.log
stderr_logfile=${APP_PATH}/storage/logs/subs-error.log
numprocs=1
stopwaitsecs=10
"
    if $DRY_RUN; then echo "[dry-run] Write $SUBS_CONF"; else printf "%s" "$SUBS_CONTENT" > "$SUBS_CONF"; fi
  fi

  if $INSTALL_APP; then
    run "mkdir -p ${APP_PATH}/storage/logs"
    run "chown -R ${WEB_USER}:${WEB_USER} ${APP_PATH}"
  fi

  run "supervisorctl reread"
  run "supervisorctl update"
  $INSTALL_APP && run "supervisorctl start nexus-worker:* || true"
  $INSTALL_APP && run "supervisorctl start nexus-worker-sync:* || true"
  $INSTALL_SUBS && run "supervisorctl start nexus-subs:* || true"
  run "supervisorctl status || true"
}

stage_cron() {
  if ! $CONFIGURE_CRON || ! $INSTALL_APP; then
    log "Stage 12: Cron scheduler skipped (CONFIGURE_CRON=false or INSTALL_APP=false)"
    return
  fi

  log "Stage 12: Cron scheduler (Laravel schedule:run)"

  local CRON_LINE="* * * * * su -s /bin/bash ${WEB_USER} -c \"/usr/bin/php ${APP_PATH}/artisan schedule:run >> ${APP_PATH}/storage/logs/cron.log 2>&1\""

  if ! grep -Fq "$CRON_LINE" /etc/crontab 2>/dev/null; then
    run "bash -c 'echo \"$CRON_LINE\" >> /etc/crontab'"
  fi
  run "systemctl restart cron || systemctl restart crond || true"
}

stage_initial_jobs() {
  if ! $RUN_INITIAL_JOBS || ! $INSTALL_APP; then
    log "Stage 13: Initial Laravel jobs skipped (RUN_INITIAL_JOBS=false or INSTALL_APP=false)"
    return
  fi

  log "Stage 13: Initial Laravel jobs"
  run "cd $APP_PATH"
  run "sudo -u ${WEB_USER} /usr/bin/php artisan military:sign-in || true"
  run "sudo -u ${WEB_USER} /usr/bin/php artisan sync:nations || true"
  run "sudo -u ${WEB_USER} /usr/bin/php artisan sync:alliances || true"
  run "sudo -u ${WEB_USER} /usr/bin/php artisan sync:wars || true"
  run "sudo -u ${WEB_USER} /usr/bin/php artisan sync:treaties || true"
  run "sudo -u ${WEB_USER} /usr/bin/php artisan taxes:collect || true"
  run "sudo -u ${WEB_USER} /usr/bin/php artisan trades:update || true"
}

stage_admin_user() {
  if ! $INSTALL_APP || ! $CREATE_ADMIN_USER_FLAG; then
    log "Stage 14: Admin user creation skipped"
    return
  fi

  if [[ "${CREATE_ADMIN_USER,,}" != "true" ]]; then
    log "Skipping admin user creation (CREATE_ADMIN_USER=false)"
    return
  fi

  log "Creating initial admin user '${ADMIN_NAME}'"
  if $DRY_RUN; then
    echo "[dry-run] Insert admin into DB $DB_DATABASE"
    return
  fi

  cd "$APP_PATH"
  HASHED_PASS=$(php -r "echo password_hash('${ADMIN_PASSWORD}', PASSWORD_BCRYPT);")
  mysql -u"${DB_USERNAME}" -p"${DB_PASSWORD}" -h"${DB_HOST}" "${DB_DATABASE}" -e "
      INSERT INTO users (name, email, password, nation_id, is_admin, verified_at, created_at, updated_at)
      VALUES ('${ADMIN_NAME}', '${ADMIN_EMAIL}', '${HASHED_PASS}', ${ADMIN_NATION_ID}, 1, NOW(), NOW(), NOW());
      SET @user_id = LAST_INSERT_ID();
      INSERT INTO role_user (user_id, role_id) VALUES (@user_id, ${ADMIN_ROLE_ID});
    " && log "Admin user created and assigned role_id=${ADMIN_ROLE_ID}" || warn "Admin creation failed; check DB creds/schema."
}

stage_redis_install_and_config() {
  if [[ "${USE_REDIS,,}" != "true" ]]; then
    log "Redis not requested (USE_REDIS=false) — skipping Redis setup"
    return
  fi

  log "Redis requested; installing and configuring"

  if $IS_DEBIAN; then
    pkg_install "redis-server"
  else
    pkg_install "redis"
  fi

  # Config maxmemory if provided
  if [[ -n "${REDIS_MAX_MEMORY:-}" ]]; then
    configure_redis_maxmemory "$REDIS_MAX_MEMORY"
  fi

  run "systemctl enable --now redis* || systemctl enable --now redis || true"
}

# ---------------------------------------------------------------------------
# RUN STAGES
# ---------------------------------------------------------------------------
INSTALL_PROFILE=${INSTALL_PROFILE:-full}
log "Using install profile: ${INSTALL_PROFILE}"

$INSTALL_BASE  && stage_base
$INSTALL_SWAP  && stage_swap
$INSTALL_PHP   && stage_php_web_stack
stage_redis_install_and_config
stage_clone_apps
stage_node_composer
stage_database_setup
stage_laravel_backend
stage_frontend_build
stage_subs_install
stage_nginx_tls
stage_supervisor
stage_cron
stage_initial_jobs
stage_admin_user

# Final perms
if $INSTALL_APP; then
  run "chown -R $WEB_USER:$WEB_USER $APP_PATH"
fi
if $INSTALL_SUBS; then
  run "chown -R $WEB_USER:$WEB_USER $SUBS_PATH"
fi

log "🎉 Installation finished."

echo ""
echo "====================  SUMMARY  ===================="
echo "Profile:             $INSTALL_PROFILE"
echo "Domain:              ${DOMAIN:-N/A}"
echo "App path:            ${APP_PATH:-N/A}"
echo "Subs path:           ${SUBS_PATH:-N/A}"
echo "Database host:       ${DB_HOST:-N/A}"
echo "Database:            ${DB_DATABASE:-N/A}"
echo "DB user:             ${DB_USERNAME:-N/A}"
echo "Redis enabled:       ${USE_REDIS:-false}"
echo "Redis maxmemory:     ${REDIS_MAX_MEMORY:-N/A}"
echo "Swap enabled:        ${ENABLE_SWAP:-true}"
echo "Swap size (GB):      ${SWAP_SIZE_GB:-4}"
echo "Certbot email:       ${CERTBOT_EMAIL:-N/A}"
echo "Cron installed:      $( $CONFIGURE_CRON && $INSTALL_APP && echo yes || echo no )"
echo "Supervisor processes:"
supervisorctl status 2>/dev/null || true
echo "Nginx test:          $(nginx -t >/dev/null 2>&1 && echo OK || echo FAIL)"
echo "Log file:            $LOG_FILE (appends each run)"
echo "==================================================="
