# Choose an installation profile

Run `sudo nexus install` on **each host** you want Nexus Setup to manage. The
installer asks for a profile number; pressing Enter selects profile 1. Each
host needs the supported OS, bootstrap, and network preparation described in
the [step-by-step guide](getting-started.md).

| Choice | Installs on this host | Database | Best for |
| --- | --- | --- | --- |
| **1. Everything on this server** | Core + Subs | Local MySQL or MariaDB | One-server installation |
| **2. Application and Subs** | Core + Subs | Existing remote database | Separate database server |
| **3. Web application** | Core | Existing remote database | Core without local Subs |
| **4. Database server only** | Database | Local MySQL or MariaDB, reachable privately | Database half of a split installation |
| **5. Subs worker only** | Subs | None locally | Subs on a separate worker |

Discord is an optional component installed **after** Core on profiles 1–3;
it is not one of the five installation profiles. [Add it with the component
commands](operations.md#install-and-control-components).

## Profile 1: everything on one server

Follow [Install Nexus step by step](getting-started.md). At `Profile [1]`,
press Enter. Enter the domain, administrator email and password, administrator
nation ID, alliance ID, Politics & War API key, and the separate Politics &
War mutation key. The installer generates the database and internal
credentials; you do not need to create them.

## Profiles 2 and 3: an existing remote database

Choose **2** for Core plus Subs, or **3** for Core alone. Before installing,
prepare a MySQL or MariaDB database that the application host can reach on
port 3306. Have its hostname or address, database name, account name, and
password ready. The account must be able to run the application's migrations
and write to that database. Restrict database access to the application host.

On the application host:

1. Run `sudo ./bootstrap.sh` from the cloned Setup repository, then
   `sudo nexus install`.
2. Choose **2** or **3**.
3. Enter the domain and the same administrator and Politics & War values as
   profile 1. Then enter the remote database host, name, user, and password.
4. Confirm the summary. After installation, run `sudo nexus status`,
   `sudo nexus doctor`, and `sudo nexus component list` on this host.

The installer uses the supplied database; it does not create an account on a
remote database. Profile 3 does not install Subs. To add Subs on the same
host later, use `sudo nexus component install subs`; that changes the managed
profile to Core plus Subs with a remote database.

## Profile 4: a separate database host

This profile creates a database and `nexus_app` account for **one named
application server**. It does not install Core, a web UI, or Subs.

1. Give the database server a private IPv4 address and know the application
   server's **different** private IPv4 address. The database address must
   already be assigned to the database host.
2. On the database host, bootstrap Setup and run `sudo nexus install`.
   Choose **4**. Enter the database host's private IPv4 address, then the
   application host's private IPv4 address. Confirm the summary.
3. Read the root-only connection file at
   `/etc/nexus/credentials/database-only.env` on the database host. It
   contains `DB_HOST`, `DB_PORT`, `DB_DATABASE`, `DB_USERNAME`, and
   `DB_PASSWORD`. Transfer those values securely to the application host;
   keep the file and password private.
4. Permit inbound TCP **3306** on the database host only from the named
   application address. Keep the database host off the public internet.
5. On the application host, bootstrap Setup and install profile **2** or **3**.
   Supply the connection details from step 3 at the remote database prompts.
   Verify the application host with `sudo nexus status` and
   `sudo nexus doctor`.

The database host binds MySQL or MariaDB to its private address and grants
`nexus_app` access from the application address entered during installation.
Operate its database and backups on that host. `nexus update` is unavailable
there because it has no application release to update; update Core and Subs
from their application hosts.

## Profile 5: a separate Subs worker

First install Core on another host. For a new split layout, choose profile
**3** on the Core host so Subs runs only on the separate worker. Make sure
Core's HTTPS URL is reachable from the Subs host. Retrieve the Core API token
from the Core host's root-controlled `/etc/nexus/nexus-core/.env`
(`NEXUS_API_TOKEN`) and transfer it securely. Also have the Politics & War
API key ready.

On the Subs host:

1. Bootstrap Setup and run `sudo nexus install`.
2. Choose **5**. For the API URL, use the Core host's HTTPS endpoint in the
   form `https://nexus.example.com/api/v1/subs`.
3. Enter the Core token and Politics & War API key, then confirm.
4. Run `sudo nexus status`, `sudo nexus doctor`, and
   `sudo nexus component list` on the Subs host.

Profile 5 has no local Core or web UI. Manage its Subs process from the **Subs
host's CLI**; the Core web UI cannot control a component on another host.
When updating a split installation, plan updates on each application or Subs
host and check health after each one. Never put the Core token in a command
argument, shell history, or a public ticket.
