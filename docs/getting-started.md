# Install Nexus step by step

This guide installs Nexus Core and Subs with a local database on one server.
For a remote database, a separate database host, or a Subs-only worker, use
the [installation profiles guide](installation-profiles.md) after reading the
preparation steps here.

## 1. Prepare the server

Use a fresh, systemd-booted **amd64** server running Ubuntu 22.04, 24.04, or
26.04 LTS, or Debian 12 or 13, and a user with `sudo` access. The installer
changes system packages, service accounts, Nginx, systemd units, and the local
database; use a host you intend to dedicate to Nexus.

Before starting:

1. Choose the domain users will visit, such as `nexus.example.com`, and point
   its DNS record to this server. Allow inbound TCP ports **80** and **443**.
   Certificate issuance happens during installation, after Core starts.
2. Make sure the host can reach GitHub Releases, Ubuntu or Debian package
   repositories, Let's Encrypt, and Politics & War. Package installation and
   release downloads require outbound access.
3. Have the administrator email, a password of at least **12 characters**, the
   administrator's numeric nation ID, the numeric alliance ID, and a Politics
   & War API key **and its separate mutation key** ready. Core uses the
   mutation key for Politics & War write actions. Do not put either secret in
   shell history or a shared issue.

## 2. Bootstrap the `nexus` command

On the server, run:

```bash
curl -fsSL https://raw.githubusercontent.com/Yosodog/Nexus-Setup/main/bootstrap.sh | sudo bash
```

Bootstrap checks the OS and architecture, downloads the latest published
`nexus-linux-amd64` release, verifies it against the release's SHA-256
checksum, installs `/usr/local/bin/nexus`, and configures the protected updater
socket and service accounts. It does **not** install Nexus Core yet. You can
check the command with `nexus version`.

To run prereleases, add `-s -- --channel beta` after `sudo bash` here and use
`sudo nexus install --channel beta` in the next step. You can also download
`bootstrap.sh`, read it, and run `sudo bash bootstrap.sh`.

## 3. Run the guided installer

```bash
sudo nexus install
```

At **Profile [1]**, press Enter for “Everything on this server.” Enter the
domain, administrator email, administrator password, administrator nation ID,
alliance ID, Politics & War API key, and Politics & War mutation key when
prompted. Review the profile and domain in the final summary, then answer
`y` to install. The password and key prompts hide input in a terminal, and
the password is asked twice. If an answer is not valid, the prompt explains why
and asks again.

Before it changes anything, the installer checks that the domain resolves to
this server, that Politics & War accepts the API key, and that the key's nation
is in the alliance and matches the administrator nation ID. An error stops the installation with
nothing changed; a warning, such as a domain that points somewhere this server
cannot reach, asks whether to continue. `--yes` skips only the warning and
final confirmations; it does not skip the required questions.

The installer creates the local `nexus` database and credentials, installs
the newest compatible Core and Subs releases, runs database migrations,
starts the services, checks their health, and requests a TLS certificate.
Wait for `Nexus installed successfully` and the list of installed releases
before continuing.

## 4. Check the installation

```bash
sudo nexus status
sudo nexus doctor
sudo nexus component list
```

`status` shows each component's release, the channel, and the profile.
`doctor` checks installation state, disk space, and managed directories. `component list` shows Core and
Subs installed and enabled; Discord is optional. Open `https://` followed by
your domain and sign in with the administrator account you entered.

Use `sudo nexus ...` immediately after bootstrap. If bootstrap was run from a
normal account with `sudo`, sign out and back in to pick up its updater group
membership; that account can then run the operating commands without `sudo`.
`nexus install` always requires `sudo`.

Continue with [operating Nexus](operations.md). If a step fails, use
[troubleshooting](troubleshooting.md) before retrying the installation.
