#!/usr/bin/env bash
# Nexus AMS installer (Ubuntu-only)
# Version: 1.0.0

set -euo pipefail

# ---- DRY RUN ---------------------------------------------------------------
DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
fi

# ---- LOGGING (append each run) --------------------------------------------
LOG_FILE="/var/log/nexus-install.log"
mkdir -p "$(dirname "$LOG_FILE")"
# Append mode tee for both stdout/stderr
exec > >(tee -a "$LOG_FILE") 2>&1

# ---- Helpers ---------------------------------------------------------------
log()  { printf "\n\033[1;32m==> %s\033[0m\n" "$*"; }
warn() { printf "\n\033[1;33m[!] %s\033[0m\n" "$*"; }
err()  { printf "\n\033[1;31m[ERROR] %s\033[0m\n" "$*"; }

run() {
  # run "cmd ..." — honors DRY_RUN
  if $DRY_RUN; then
    echo "[dry-run] $*"
  else
    eval "$@"
  fi
}

die() { err "$1"; exit 1; }

require_root() { [[ $EUID -eq 0 ]] || die "Run as root (sudo)."; }
require_ubuntu() {
  [[ -f /etc/os-release ]] || die "/etc/os-release not found. Unsupported OS."
  . /etc/os-release
  [[ "${ID:-}" == "ubuntu" ]] || die "This installer supports Ubuntu only."
}

# ---- Preconditions ---------------------------------------------------------
require_root
require_ubuntu

# env file required (no interactive prompts)
ENV_PATH="./install.env"
[[ -f "$ENV_PATH" ]] || die "install.env not found next to the script. Create it and re-run."

# shellcheck disable=SC1090
source "$ENV_PATH"

# Derived
PHP_FPM_SOCK="/run/php/php8.4-fpm.sock"
NGINX_SITE="/etc/nginx/sites-available/default"
NGINX_ENABLED="/etc/nginx/sites-enabled/default"

log "Nexus AMS Installer v1.0.0 (Ubuntu-only)"
$DRY_RUN && warn "Dry-run mode enabled. Commands will be printed but not executed."

# ---- Port checks -----------------------------------------------------------
log "Checking if ports 80/443 are already in use"
if ss -tulpn | grep -q ":80 "; then warn "Port 80 appears in use. Continuing (Nginx will likely reuse it)."; fi
if ss -tulpn | grep -q ":443 "; then warn "Port 443 appears in use. Continuing (Nginx/Certbot will handle)."; fi

# ---- Stage 1: System prep --------------------------------------------------
log "Stage 1/12: System update & base packages"
run "apt update && apt upgrade -y"
run "apt install -y software-properties-common curl git unzip lsb-release ca-certificates apt-transport-https gnupg2"

log "Stage 2/12: Swap (4G, idempotent)"
if ! swapon --show | grep -q "/swapfile"; then
  run "fallocate -l 4G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=4096 status=progress"
  run "chmod 600 /swapfile"
  run "mkswap /swapfile"
  run "swapon /swapfile"
  grep -q "/swapfile" /etc/fstab || run "echo '/swapfile none swap sw 0 0' >> /etc/fstab"
else
  log "Swap already present — skipping"
fi

# ---- Stage 3: PHP, MySQL, Nginx -------------------------------------------
log "Stage 3/12: PHP 8.4, MySQL, Nginx"
if ! ls /etc/apt/sources.list.d/ 2>/dev/null | grep -q "ondrej-ubuntu-php"; then
  run "add-apt-repository ppa:ondrej/php -y"
  run "apt update"
fi
run "apt install -y php8.4 php8.4-cli php8.4-fpm php8.4-mysql php8.4-xml php8.4-curl php8.4-mbstring php8.4-zip php8.4-bcmath"

run "apt install -y mysql-server"
run "systemctl enable --now mysql"

run "apt install -y nginx"
run "systemctl enable --now nginx"

# ---- Stage 4: Clone apps ---------------------------------------------------
log "Stage 4/12: Clone Nexus AMS and Subs"
run "mkdir -p $(dirname "$APP_PATH")"
if [[ ! -d "$APP_PATH/.git" ]]; then
  run "cd $(dirname "$APP_PATH") && git clone https://github.com/Yosodog/Nexus-AMS.git"
  if [[ "$APP_PATH" != "$(dirname "$APP_PATH")/Nexus-AMS" ]]; then
    run "mv $(dirname "$APP_PATH")/Nexus-AMS $APP_PATH"
  fi
fi

run "mkdir -p $(dirname "$SUBS_PATH")"
if [[ ! -d "$SUBS_PATH/.git" ]]; then
  run "cd $(dirname "$SUBS_PATH") && git clone https://github.com/Yosodog/Nexus-AMS-Subs.git"
  if [[ "$SUBS_PATH" != "$(dirname "$SUBS_PATH")/Nexus-AMS-Subs" ]]; then
    run "mv $(dirname "$SUBS_PATH")/Nexus-AMS-Subs $SUBS_PATH"
  fi
fi

# ---- Stage 5: Node & Composer ---------------------------------------------
log "Stage 5/12: Node LTS & Composer"
run "curl -fsSL https://deb.nodesource.com/setup_lts.x | bash -"
run "apt install -y nodejs"
run "php -r \"copy('https://getcomposer.org/installer','composer-setup.php');\""
run "php composer-setup.php --install-dir=/usr/local/bin --filename=composer"
run "php -r \"unlink('composer-setup.php');\""

# ---- Stage 6: Database -----------------------------------------------------
log "Stage 6/12: Create MySQL DB & user (idempotent)"
MYSQL_CMD="mysql -uroot"
if ! $DRY_RUN; then
  if ! $MYSQL_CMD -e "SELECT 1" &>/dev/null; then
    warn "Root socket login failed; trying sudo mysql"
    MYSQL_CMD="sudo mysql"
  fi
fi
run "$MYSQL_CMD <<SQL
CREATE DATABASE IF NOT EXISTS \`$DB_DATABASE\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS '$DB_USERNAME'@'localhost' IDENTIFIED BY '$DB_PASSWORD';
GRANT ALL PRIVILEGES ON \`$DB_DATABASE\`.* TO '$DB_USERNAME'@'localhost';
FLUSH PRIVILEGES;
SQL"

# ---- Stage 7: Laravel .env & backend setup --------------------------------
log "Stage 7/12: Configure Laravel .env and install backend"
run "cd $APP_PATH"
if [[ ! -f "$APP_PATH/.env" ]]; then run "cp .env.example .env"; fi

set_env() {
  local key="$1" val="$2"
  if $DRY_RUN; then
    echo "[dry-run] set $key=$val in $APP_PATH/.env"
    return
  fi
  if grep -q "^$key=" .env; then
    sed -i "s|^$key=.*|$key=${val//|/\\|}|" .env
  else
    echo "$key=$val" >> .env
  fi
}

set_env "APP_NAME" "\"$APP_NAME\""
set_env "APP_ENV" "production"
set_env "APP_DEBUG" "false"
set_env "APP_URL" "$APP_URL"
set_env "DB_CONNECTION" "mysql"
set_env "DB_HOST" "127.0.0.1"
set_env "DB_PORT" "3306"
set_env "DB_DATABASE" "$DB_DATABASE"
set_env "DB_USERNAME" "$DB_USERNAME"
set_env "DB_PASSWORD" "$DB_PASSWORD"
set_env "PW_API_KEY" "$PW_API_KEY"
set_env "PW_API_MUTATION_KEY" "$PW_API_MUTATION_KEY"
set_env "NEXUS_API_TOKEN" "$NEXUS_API_TOKEN"
set_env "PW_ALLIANCE_ID" "$PW_ALLIANCE_ID"

run "chmod 600 $APP_PATH/.env"

run "composer install --no-dev --optimize-autoloader"
run "php artisan key:generate --force"
run "php artisan migrate --force"
run "php artisan db:seed --force"

# ---- Stage 8: Frontend build ----------------------------------------------
log "Stage 8/12: Frontend deps & Vite build"
run "npm ci"
# make esbuild binary executable if present (various arch folders)
if ! $DRY_RUN; then
  find "$APP_PATH/node_modules" -path "*/@esbuild/*/bin/esbuild" -type f -exec chmod +x {} \; 2>/dev/null || true
fi
run "npm run build"
run "chown -R www-data:www-data $APP_PATH/public/build"

# ---- Stage 9: Subs .env & install -----------------------------------------
log "Stage 9/12: Configure Subs .env & install"
run "cd $SUBS_PATH"
if [[ ! -f "$SUBS_PATH/.env" ]]; then run "cp .env.example .env"; fi

set_subs() {
  local key="$1" val="$2"
  if $DRY_RUN; then
    echo "[dry-run] set $key=$val in $SUBS_PATH/.env"
    return
  fi
  if grep -q "^$key=" .env; then
    sed -i "s|^$key=.*|$key=${val//|/\\|}|" .env
  else
    echo "$key=$val" >> .env
  fi
}

set_subs "PW_API_TOKEN" "$PW_API_TOKEN"
set_subs "NEXUS_API_URL" "$NEXUS_API_URL"
set_subs "NEXUS_API_TOKEN" "$NEXUS_API_TOKEN"
set_subs "ENABLE_SNAPSHOTS" "$ENABLE_SNAPSHOTS"

run "npm ci"
run "chown -R www-data:www-data $SUBS_PATH"

# ---- Stage 10: Nginx vhost + TLS ------------------------------------------
log "Stage 10/12: Nginx vhost for $DOMAIN + Certbot"
# Write site
SITE_BLOCK="
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
        include snippets/fastcgi-php.conf;
        fastcgi_pass unix:${PHP_FPM_SOCK};
        fastcgi_param SCRIPT_FILENAME \$realpath_root\$fastcgi_script_name;
        include fastcgi_params;
    }

    location ~ /\. { deny all; }

    add_header X-Frame-Options 'SAMEORIGIN';
    add_header X-Content-Type-Options 'nosniff';
    add_header Referrer-Policy 'strict-origin-when-cross-origin';
    add_header X-XSS-Protection '1; mode=block';

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
# ensure enabled symlink
if [[ ! -L "$NGINX_ENABLED" ]]; then
  run "ln -sf $NGINX_SITE $NGINX_ENABLED"
fi
run "nginx -t"
run "systemctl reload nginx"

# Install certbot & issue cert (unattended)
run "apt install -y certbot python3-certbot-nginx"
run "certbot --nginx -d $DOMAIN --non-interactive --agree-tos -m $ADMIN_EMAIL --redirect || true"
run "certbot renew --dry-run || true"
run "nginx -t && systemctl reload nginx"

# ---- Stage 11: Supervisor + Cron ------------------------------------------
log "Stage 11/12: Supervisor processes & cron scheduler"
run "apt install -y supervisor"
run "systemctl enable --now supervisor"

# Laravel worker
WORKER_CONF="/etc/supervisor/conf.d/nexus-worker.conf"
WORKER_CONTENT="[program:nexus-worker]
process_name=%(program_name)s_%(process_num)02d
directory=${APP_PATH}
command=/usr/bin/php artisan queue:work --queue=default --sleep=3 --tries=3 --max-time=3600
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
numprocs=2
user=www-data
redirect_stderr=true
stdout_logfile=${APP_PATH}/storage/logs/worker.log
stopwaitsecs=10
"
if $DRY_RUN; then echo "[dry-run] Write $WORKER_CONF"; else printf "%s" "$WORKER_CONTENT" > "$WORKER_CONF"; fi

# Sync queue worker (heavy nation/war sync jobs)
WORKER_SYNC_CONF="/etc/supervisor/conf.d/nexus-worker-sync.conf"
WORKER_SYNC_CONTENT="[program:nexus-worker-sync]
process_name=%(program_name)s_%(process_num)02d
directory=${APP_PATH}
command=/usr/bin/php artisan queue:work --queue=sync --sleep=3 --tries=3 --max-time=3600
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
numprocs=1
user=www-data
redirect_stderr=true
stdout_logfile=${APP_PATH}/storage/logs/worker-sync.log
stopwaitsecs=10
"
if $DRY_RUN; then echo "[dry-run] Write $WORKER_SYNC_CONF"; else printf "%s" "$WORKER_SYNC_CONTENT" > "$WORKER_SYNC_CONF"; fi


# Subs (entry in src/index.js)
SUBS_CONF="/etc/supervisor/conf.d/nexus-subs.conf"
SUBS_CONTENT="[program:nexus-subs]
directory=${SUBS_PATH}/src
command=/usr/bin/node index.js
autostart=true
autorestart=true
stopasgroup=true
killasgroup=true
user=www-data
environment=NODE_ENV=\"production\",PATH=\"/usr/bin\"
stdout_logfile=${APP_PATH}/storage/logs/subs.log
stderr_logfile=${APP_PATH}/storage/logs/subs-error.log
numprocs=1
stopwaitsecs=10
"
if $DRY_RUN; then echo "[dry-run] Write $SUBS_CONF"; else printf "%s" "$SUBS_CONTENT" > "$SUBS_CONF"; fi

run "mkdir -p ${APP_PATH}/storage/logs"
run "chown -R www-data:www-data ${APP_PATH}"

run "supervisorctl reread"
run "supervisorctl update"
run "supervisorctl start nexus-worker:* || true"
run "supervisorctl start nexus-worker-sync:* || true"
run "supervisorctl start nexus-subs:* || true"
run "supervisorctl status || true"

# Cron (www-data scheduler)
CRON_LINE="* * * * * su -s /bin/bash www-data -c "/usr/bin/php /var/www/nexus/artisan schedule:run >> /var/www/nexus/storage/logs/cron.log 2>&1""
if ! grep -Fq "$CRON_LINE" /etc/crontab; then
  run "bash -c 'echo \"$CRON_LINE\" >> /etc/crontab'"
fi
run "systemctl restart cron || systemctl restart crond || true"

# ---- Stage 12: Initial jobs (order per your guidance) ----------------------
log "Stage 12/12: Initial Laravel jobs"
run "cd $APP_PATH"
run "sudo -u www-data /usr/bin/php artisan military:sign-in || true"
run "sudo -u www-data /usr/bin/php artisan sync:nations || true"
run "sudo -u www-data /usr/bin/php artisan sync:alliances || true"
run "sudo -u www-data /usr/bin/php artisan sync:wars || true"
run "sudo -u www-data /usr/bin/php artisan sync:treaties || true"
run "sudo -u www-data /usr/bin/php artisan taxes:collect || true"
run "sudo -u www-data /usr/bin/php artisan trades:update || true"

# ---- Stage 13: Optional admin user creation -------------------------------
if [[ "${CREATE_ADMIN_USER,,}" == "true" ]]; then
  log "Creating initial admin user '${ADMIN_NAME}'"
  if $DRY_RUN; then
    echo "[dry-run] Insert admin into DB $DB_DATABASE"
  else
    cd "$APP_PATH"
    HASHED_PASS=$(php -r "echo password_hash('${ADMIN_PASSWORD}', PASSWORD_BCRYPT);")
    mysql -u"${DB_USERNAME}" -p"${DB_PASSWORD}" "${DB_DATABASE}" -e "
      INSERT INTO users (name, email, password, nation_id, is_admin, verified_at, created_at, updated_at)
      VALUES ('${ADMIN_NAME}', '${ADMIN_EMAIL}', '${HASHED_PASS}', ${ADMIN_NATION_ID}, 1, NOW(), NOW(), NOW());
      SET @user_id = LAST_INSERT_ID();
      INSERT INTO role_user (user_id, role_id) VALUES (@user_id, ${ADMIN_ROLE_ID});
    " && log "Admin user created and assigned role_id=${ADMIN_ROLE_ID}" || warn "Admin creation failed; check DB creds/schema."
  fi
else
  log "Skipping admin user creation (CREATE_ADMIN_USER=false)"
fi

# ---- Final perms & summary -------------------------------------------------
run "chown -R www-data:www-data $APP_PATH $SUBS_PATH"

log "🎉 Installation finished."

echo ""
echo "====================  SUMMARY  ===================="
echo "Domain:               $DOMAIN"
echo "App path:             $APP_PATH"
echo "Subs path:            $SUBS_PATH"
echo "Database:             $DB_DATABASE"
echo "DB user:              $DB_USERNAME"
echo "Certbot email:        $ADMIN_EMAIL"
echo "Cron installed:       yes (www-data schedule:run)"
echo "Supervisor processes: "
supervisorctl status || true
echo "Nginx test:           $(nginx -t >/dev/null 2>&1 && echo OK || echo FAIL)"
echo "Log file:             $LOG_FILE (appends each run)"
echo "==================================================="