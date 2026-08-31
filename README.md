# Nexus AMS installer for Ubuntu

Nexus Setup installs the self-hosted Nexus AMS beta on Ubuntu. It prepares the web application, database, scheduler, queue workers, and the Politics & War subscription service on one server or across the supported split-host profiles.

## Beta status

Nexus AMS, Nexus Setup, Nexus AMS Subs, and Nexus AMS Discord are in beta. Pin compatible revisions for AMS and Subs. Production and noninteractive installs require exact commit SHAs so a later push to `main` cannot change an existing installation.

The installer supports standalone deployments. Nexus Cloud onboarding is still under development and uses a separate managed deployment path.

## System requirements

| Requirement | Description |
|--------------|-------------|
| Operating System | Ubuntu 22.04 LTS or later |
| PHP | 8.3 with bcmath, curl, DOM, GD, intl, mbstring, MySQL, Redis when selected, and ZIP extensions |
| Node.js | 22 LTS |
| Database | MySQL 8.0 or a compatible MariaDB release |
| Redis | Optional; 6.2 or newer is required for Redis Streams delivery |
| Access | Root or sudo privileges |
| Ports | 80 and 443 open to the public |
| Memory | Minimum 2 GB (4 GB+ recommended) |
| Disk Space | Minimum 5 GB free |
| Internet | Required for apt packages, GitHub access, and SSL issuance |

## What the installer does

1. Updates and upgrades system packages.
2. Configures an optional 4 GB swap file.
3. Installs PHP 8.3, the selected database, Nginx, Node.js 22, Composer, Supervisor, and Certbot.
4. Checks out Nexus AMS and Nexus AMS Subs at the configured commits.
5. Creates the database and writes the application environment files.
6. Installs backend and frontend dependencies, runs migrations and seeders, and builds the Vite assets.
7. Configures Nginx, TLS, queue workers, the subscription service, and the Laravel scheduler.
8. Optionally creates the first administrator and runs the initial synchronization jobs.

All output is logged to `/var/log/nexus-install.log`.

## Repository layout

```
/install_nexus.sh     # Main installation script
/installer_helpers.sh # Tested defaults, provenance, HMAC, and safe environment writes
/install.env          # Configuration file (must be edited before running)
/tests/install_contract_test.sh # Standalone compatibility checks
README.md
LICENSE

```

## Quick start

Clone this repository onto your Ubuntu server. For a beta installation, check out a tagged Nexus Setup beta before running the installer.

```bash
git clone https://github.com/Yosodog/Nexus-Setup.git
cd Nexus-Setup
chmod +x install_nexus.sh
```

Edit the environment file with your deployment values:

```bash
chmod 600 install.env
nano install.env

```

Then run the installer:

```bash
# Set NEXUS_AMS_COMMIT, NEXUS_SUBS_COMMIT, and NEXUS_RELEASE_ID first.
./install_nexus.sh --check-config
sudo ./install_nexus.sh --non-interactive

```

To preview actions without making changes, use:

```bash
sudo ./install_nexus.sh --non-interactive --dry-run

```

## Environment configuration (`install.env`)

This file defines all required installation variables.  
Inline comments are supported; lines beginning with `#` are ignored.

```bash
# ========== SYSTEM VARIABLES ==========
DOMAIN="nexus.example.com"
APP_PATH="/var/www/nexus"
SUBS_PATH="/var/nexus-subs"
INSTALL_PROFILE="full" # full, app-web-subs-remote-db, web-only, db-only, or subs-only
ENABLE_SWAP="true"
SWAP_SIZE_GB="4"
NODE_BUILD_MAX_OLD_SPACE="2048"
NEXUS_RUNTIME="standalone"
NEXUS_MANAGED="false"
NEXUS_AMS_REPOSITORY="https://github.com/Yosodog/Nexus-AMS.git"
NEXUS_AMS_COMMIT="" # Full 40-character SHA required for production/noninteractive AMS installs
NEXUS_SUBS_REPOSITORY="https://github.com/Yosodog/Nexus-AMS-Subs.git"
NEXUS_SUBS_COMMIT="" # Full 40-character SHA required for production/noninteractive Subs installs
NEXUS_RELEASE_ID="" # Required for production/noninteractive Subs installs
ALLOW_UNPINNED_DEVELOPMENT="false" # Interactive-only local development escape hatch

# ========== DATABASE ==========
DB_HOST="127.0.0.1"
DB_DATABASE="nexus_ams"
DB_USERNAME="nexususer"
DB_PASSWORD="replace-with-a-strong-password"

# ========== CACHE / QUEUE / SESSION ==========
USE_REDIS="false"
REDIS_MAX_MEMORY=""

# ========== LARAVEL ENV VARIABLES ==========
APP_NAME="Nexus AMS" # Change to what you want your application to be called
APP_URL="https://nexus.example.com" # Full URL of your Nexus app
PW_API_KEY="your_pw_api_key_here" 
PW_API_MUTATION_KEY="your_pw_api_mutation_key_here" # Politics & War mutation key
NEXUS_API_TOKEN="replace-with-a-random-shared-token"
PW_ALLIANCE_ID="1234" # Your primary alliance ID

# ========== NEXUS SUBS ENV VARIABLES ==========
PW_API_TOKEN="your_pw_api_key_here" # May use a separate key from the AMS application
NEXUS_API_URL="https://nexus.example.com/api/v1/subs"
ENABLE_SNAPSHOTS="false" # Snapshot support remains disabled in the beta
SUBS_DELIVERY_DRIVER="http" # http or redis-stream
SUBS_REDIS_URL="" # For a local full install use redis://127.0.0.1:6379/3
SUBS_REDIS_STREAM="nexus:subscriptions:v1"
SUBS_REDIS_HMAC_SECRET="" # Required for redis-stream; use the same 32+ character secret on AMS and Subs
SUBS_REDIS_GROUP="nexus-ams"
SUBS_REDIS_BLOCK_MS="5000"
SUBS_REDIS_READ_COUNT="10"
SUBS_REDIS_CLAIM_IDLE_MS="60000"
SUBS_REDIS_MAX_DELIVERIES="5"

# ========== ADMIN EMAIL FOR CERTBOT ==========
CERTBOT_EMAIL="admin@example.com"

# ========== NEXUS ADMIN USER CREATION ==========
CREATE_ADMIN_USER="true"             # set to false to skip
ADMIN_NAME="Nexus Admin"
ADMIN_EMAIL="admin@example.com"
ADMIN_PASSWORD="change-me-now" # Your password (please change after install)
ADMIN_NATION_ID="123456" # Your nation ID
ADMIN_ROLE_ID="1" # If this is a fresh install, ID 1 will be the default admin role

```

Legacy noninteractive files may omit the runtime, profile, database-host,
Redis, and newer stream settings. They default to `standalone`, `full`,
`127.0.0.1`, disabled local Redis, and HTTP Subs delivery, preserving the
pre-Cloud install path. `PW_API_TOKEN` defaults to `PW_API_KEY` only when a
separate Subs key was not configured. Repository commits and
`NEXUS_RELEASE_ID` are intentionally not defaulted: production and
noninteractive installs fail closed until immutable provenance is supplied.

Interactive development may set `ALLOW_UNPINNED_DEVELOPMENT=true`. That mode is
never accepted by `--non-interactive` or `--check-config`; it records the
actual checked-out Subs commit and derives a `development-<short-sha>` release
ID. Do not use that mode for production.

## Standalone runtime compatibility

Nexus Setup installs self-hosted Nexus AMS only. It writes
`NEXUS_RUNTIME=standalone` and `NEXUS_MANAGED=false`, never requests a Nexus
Cloud account, and never configures Cloud tenant IDs, callbacks, bootstrap
introspection, Cloud sessions, roles, or memberships. Public synchronization,
tenant-private schedules and workflows, local backups, Discord integration,
and HTTP or Redis Subs delivery remain owned by the standalone AMS install.

`hosted-tenant`, `world-writer`, any non-false `NEXUS_MANAGED` value, and any
non-false `NEXUS_TENANT_EVENTS_ENABLED` value fail configuration validation
before packages, databases, files, or provider endpoints are changed. A fresh
environment is normalized to standalone settings even if a future AMS
`.env.example` contains hosted defaults. The installer rejects those hosted
callback, bootstrap, tenant-event key, consumer, and Redis values in both its
input `install.env` and an existing AMS `.env`; it also refuses a managed runtime
or enabled hosted tenant-event consumption. Cloud onboarding is not public yet.
Managed pilots use the Nexus Cloud deployment agent and immutable image path
instead of this installer.

Before a fresh install or upgrade, run:

```bash
./install_nexus.sh --check-config
bash -n install_nexus.sh installer_helpers.sh tests/install_contract_test.sh
bash tests/install_contract_test.sh
```

The config check does not require root and does not contact external providers.
On standalone upgrades, Laravel migrations continue targeting physical local
world tables; Nexus Setup does not install hosted world views or Cloud
credentials. Back up the application and database before applying an upgrade.

----------

## Example run summary

At completion, you should see a summary similar to:

```
Installation finished.

====================  SUMMARY  ====================
Domain:               example.com
App path:             /var/www/nexus
Subs path:            /var/nexus-subs
Database:             nexus_ams
DB user:              nexususer
Certbot email:        admin@example.com
Cron installed:       yes (www-data schedule:run)
Supervisor processes:
nexus-worker:RUNNING
nexus-subs:RUNNING
nexus-subs-stream:RUNNING # when SUBS_DELIVERY_DRIVER=redis-stream
Nginx test:           OK
Log file:             /var/log/nexus-install.log
===================================================
```

----------

## After installation

-   Visit your site at: `https://your-domain/`
    
-   Log in using your admin credentials.
    
-   Check Supervisor status:
    
    ```bash
    sudo supervisorctl status
    
    ```

-   Confirm scheduler cron entry:
    
    ```bash
    grep artisan /etc/crontab
    
    ```
    
-   Review logs:
    
    ```bash
    less /var/log/nexus-install.log
    
    ```

## Optional Redis Streams delivery

The subscription service uses HTTP by default. Set
`SUBS_DELIVERY_DRIVER=redis-stream` to publish Politics & War deliveries to a
Redis Stream and run the Nexus `subs:consume-stream` worker.

For a full installation with `USE_REDIS=true`, the default stream endpoint is
`redis://127.0.0.1:6379/3`. Redis DB 0 remains available for queues, DB 1 for
cache, and DB 2 for Pulse. The installer writes Laravel's current
`CACHE_STORE=redis` setting; without Redis it writes `CACHE_STORE=file`.
Successful stream messages are acknowledged and deleted, so DB 3 is transport
rather than long-term event storage.

For split `web-only` and `subs-only` installations, configure both hosts with
the same private `redis://` or TLS `rediss://` URL. The installer deliberately
does not change Redis bind addresses or open port 6379. Provision private
networking, TLS, authentication, ACLs, and firewall rules outside this script.

Redis Stream entries are authenticated with HMAC-SHA256. Configure the same
`SUBS_REDIS_HMAC_SECRET` in each split host's `install.env`; it must contain at
least 32 characters. A combined interactive installation generates a 256-bit
secret when the prompt is left blank. The installer writes the same secret to
both AMS and Subs `.env` files, keeps environment files mode `0600`, and
redacts secret assignments in dry-run output. The HMAC value is never printed.

The generated Subs `.env` also receives `BUILD_COMMIT` from the checked-out
Subs repository and the configured `NEXUS_RELEASE_ID`. The installer verifies
the actual checkout matches the requested full SHA before writing that
metadata.

The HTTP URL and token remain configured as a manual backup. To roll back:

1. Stop `nexus-subs` so no new stream messages are published.
2. Confirm the stream and pending list are drained or accounted for.
3. Set `DELIVERY_DRIVER=http` in the Subs `.env`.
4. Restart `nexus-subs` and confirm HTTP delivery logs resume.

Do not run HTTP and Redis publishing simultaneously. Automatic fallback is not
enabled because an ambiguous Redis timeout could otherwise deliver the same
event through both transports.

Redis Streams delivery requires Redis 6.2 or newer. Local Redis installations
retain AOF persistence and the `noeviction` memory policy. The installer also
configures log rotation for producer and consumer dead-letter files.
    

----------

## Troubleshooting

| Problem                              | Possible Cause / Fix                                                                      |
| ------------------------------------ | ----------------------------------------------------------------------------------------- |
| **403 Forbidden or PHP downloading** | PHP-FPM not linked; rerun installer or verify `/run/php/php8.3-fpm.sock` in Nginx config. |
| **Certbot failure**                  | Ensure ports 80/443 are open and DNS resolves to this server.                             |
| **Vite build error (EACCES)**        | Run `chmod +x node_modules/@esbuild/linux-x64/bin/esbuild`.                               |
| **Supervisor not starting**          | `sudo systemctl restart supervisor` and check `/var/log/supervisor/supervisord.log`.      |
| **MySQL root access denied**         | Use `sudo mysql` to verify socket authentication.                                         |

## Uninstallation

Remove all components manually if needed:

```bash
sudo systemctl stop nginx mysql php8.3-fpm supervisor
sudo apt purge -y nginx mysql-server php8.3* supervisor certbot
sudo rm -rf /var/www/nexus /var/nexus-subs
sudo rm -rf /etc/supervisor/conf.d/nexus-*
sudo rm -rf /etc/letsencrypt/live/nexus.example.com
sudo rm /swapfile
sudo rm -f /var/log/nexus-install.log

```

## Development and contributions

Pull requests and issue reports are welcome. Include testing notes when changing the installer or adding support for another Linux distribution.

## License

This project is licensed under the **GNU General Public License v3.0 (GPL-3.0)**.  
See the LICENSE file for the full text.
