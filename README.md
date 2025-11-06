# Nexus AMS — Automated Installer (Ubuntu)

This repository provides a fully automated installation script for **Nexus AMS**, a Laravel-based Alliance Management System for Politics & War.  
The installer sets up a complete production environment on Ubuntu, including:

-   PHP 8.4 (via Ondřej Surý PPA)
    
-   MySQL Server
    
-   Nginx
    
-   Node.js (LTS)
    
-   Composer
    
-   Supervisor
    
-   Let’s Encrypt (Certbot) SSL
    
-   Automatic Laravel scheduling and background workers
    

The goal is a one-command, unattended deployment of a ready-to-use Nexus AMS instance.


## System Requirements

| Requirement | Description |
|--------------|-------------|
| Operating System | Ubuntu 22.04 LTS or later |
| Access | Root or sudo privileges |
| Ports | 80 and 443 open to the public |
| Memory | Minimum 2 GB (4 GB+ recommended) |
| Disk Space | Minimum 5 GB free |
| Internet | Required for apt packages, GitHub access, and SSL issuance |

## What the Installer Does

1.  Updates and upgrades all packages.
    
2.  Configures a 4 GB swap file (idempotent).
    
3.  Installs PHP 8.4, MySQL, and Nginx.
    
4.  Clones both the **Nexus AMS** and **Nexus-AMS-Subs** repositories.
    
5.  Installs Node LTS and Composer.
    
6.  Creates the MySQL database and user.
    
7.  Generates Laravel and Subs `.env` files from their examples.
    
8.  Runs `composer install`, `php artisan migrate`, and `db:seed`.
    
9.  Builds front-end assets with Vite.
    
10.  Configures Nginx with SSL certificates from Let’s Encrypt.
    
11.  Installs and configures Supervisor for queue workers and subs service.
    
12.  Adds a cron entry for the Laravel scheduler (`www-data`).
    
13.  Optionally creates an initial admin user.
    
14.  Runs initial sync jobs (`military:sign-in`, `sync:nations`, etc.).
    

All output is logged to `/var/log/nexus-install.log`.

## Repository Layout

```
/install_nexus.sh     # Main installation script
/install.env          # Configuration file (must be edited before running)
README.md
LICENSE

```

## Quick Start (GitHub Installation)

Clone this repository onto your Ubuntu server and run the installer.

```bash
# Clone the installer
git clone https://github.com/Yosodog/Nexus-Setup.git
cd Nexus-Setup

# Make the script executable
chmod +x install_nexus.sh

```

Edit the environment file with your deployment values:

```bash
nano install.env

```

Then run the installer:

```bash
sudo ./install_nexus.sh

```

To preview actions without making changes, use:

```bash
sudo ./install_nexus.sh --dry-run

```

## Environment Configuration (`install.env`)

This file defines all required installation variables.  
Inline comments are supported; lines beginning with `#` are ignored.

```bash
# ========== SYSTEM VARIABLES ==========
DOMAIN="example.com" # Input your root domain (ex: nexus.bkpw.net)
APP_PATH="/var/www/nexus"
SUBS_PATH="/var/www/Nexus-AMS-Subs"

# ========== DATABASE ==========
DB_DATABASE="nexus_ams"
DB_USERNAME="nexususer"
DB_PASSWORD="StrongPasswordHere!" # Please change this

# ========== LARAVEL ENV VARIABLES ==========
APP_NAME="Nexus AMS" # Change to what you want your application to be called
APP_URL="https://example.com" # Full URL of your Nexus app
PW_API_KEY="your_pw_api_key_here" 
PW_API_MUTATION_KEY="your_pw_api_mutation_key_here" # The special "bot" key
NEXUS_API_TOKEN="your_nexus_api_token_here" # Key used to secure your Nexus Subs API endpoints. Generate something random
PW_ALLIANCE_ID="877" # Your primary alliance ID

# ========== NEXUS SUBS ENV VARIABLES ==========
PW_API_TOKEN="your_pw_api_key_here" # This could be different than above, so that's why it's here
NEXUS_API_URL="https://example.com/api/v1/subs" # No ending /, use your domain to start it
ENABLE_SNAPSHOTS="false" # Should leave to false until we can get snapshots to work properly

# ========== ADMIN EMAIL FOR CERTBOT ==========
ADMIN_EMAIL="admin@example.com"

# ========== NEXUS ADMIN USER CREATION ==========
CREATE_ADMIN_USER="true"             # set to false to skip
ADMIN_NAME="Yosodog" # Your username
ADMIN_EMAIL="yosodog@example.com" # Your email
ADMIN_PASSWORD="ilovechicken123" # Your password (please change after install)
ADMIN_NATION_ID="10472" # Your nation ID
ADMIN_ROLE_ID="1" # If this is a fresh install, ID 1 will be the default admin role

```

----------

## Example Run Summary

At completion, you should see a summary similar to:

```
Installation finished.

====================  SUMMARY  ====================
Domain:               example.com
App path:             /var/www/nexus
Subs path:            /var/www/Nexus-AMS-Subs
Database:             nexus_ams
DB user:              nexususer
Certbot email:        admin@example.com
Cron installed:       yes (www-data schedule:run)
Supervisor processes:
nexus-worker:RUNNING
nexus-subs:RUNNING
Nginx test:           OK
Log file:             /var/log/nexus-install.log
===================================================

```

----------

## Post-Installation

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
    

----------

## Troubleshooting

| Problem                              | Possible Cause / Fix                                                                      |
| ------------------------------------ | ----------------------------------------------------------------------------------------- |
| **403 Forbidden or PHP downloading** | PHP-FPM not linked; rerun installer or verify `/run/php/php8.4-fpm.sock` in Nginx config. |
| **Certbot failure**                  | Ensure ports 80/443 are open and DNS resolves to this server.                             |
| **Vite build error (EACCES)**        | Run `chmod +x node_modules/@esbuild/linux-x64/bin/esbuild`.                               |
| **Supervisor not starting**          | `sudo systemctl restart supervisor` and check `/var/log/supervisor/supervisord.log`.      |
| **MySQL root access denied**         | Use `sudo mysql` to verify socket authentication.                                         |

## Uninstallation

Remove all components manually if needed:

```bash
sudo systemctl stop nginx mysql php8.4-fpm supervisor
sudo apt purge -y nginx mysql-server php8.4* supervisor certbot
sudo rm -rf /var/www/nexus /var/www/Nexus-AMS-Subs
sudo rm -rf /etc/supervisor/conf.d/nexus-*
sudo rm -rf /etc/letsencrypt/live/nexus.bkpw.net
sudo rm /swapfile
sudo rm -f /var/log/nexus-install.log

```

## Development and Contributions

Pull requests and issue reports are welcome.  
If you enhance automation or support additional distros versions, please include detailed testing notes in your PR description.

## License

This project is licensed under the **GNU General Public License v3.0 (GPL-3.0)**.  
See the LICENSE file for the full text.
