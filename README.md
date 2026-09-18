# Tunnel Manager

[한국어](docs/README.ko.md)

Tunnel Manager opens SSH tunnels and keeps them open. You register the SSH
servers it should log in to (a **Host**) and the services it should publish (a
**service port**), and it builds one tunnel for every combination of the two and
watches it from then on. It is a single binary that serves a REST API, a browser
UI and the tunnels themselves.

The tunnels are **reverse** tunnels, which means the listening socket is opened
on the Host and not on the machine Tunnel Manager runs on. A client that
connects to `local_port` **on the Host** is carried through the SSH connection to
Tunnel Manager, which then connects to `service_ip:service_port` and copies the
bytes in both directions. That is how a service only Tunnel Manager can reach is
made reachable from the Host.

| What you register | Fields | What it is |
|-------------------|--------|------------|
| Host | `ip`, `port`, `user`, `password`, `description`, `enabled` | An SSH server Tunnel Manager logs in to with a username and a password. The password is stored encrypted. |
| Service port | `service_ip`, `service_port`, `local_port` | The service to publish, and the port opened on every Host to reach it. |

Both `ip` and `service_ip` take an IPv4 or an IPv6 address. An IPv6 address is
written plainly, as `2001:db8::1`, and the brackets a dialer needs are put on
where the address is used. A zone, as in `fe80::1%eth0`, is refused.

Every **enabled** Host combined with every service port is one tunnel. Two
enabled Hosts and three service ports means six tunnels.

**There is nothing to install beside the binary.** The database is a SQLite file
the process creates itself, the settings are kept in that file and are changed on
a screen in the browser, and the UI is compiled into the executable. No database
server, no configuration file, no directory that has to travel next to it.

## Contents

- [Requirements](#requirements)
- [How it works](#how-it-works)
- [Install and run](#install-and-run)
- [First startup and the account](#first-startup-and-the-account)
- [The built-in UI](#the-built-in-ui)
- [Settings](#settings)
- [Uninstall](#uninstall)
- [Calling the API from a script](#calling-the-api-from-a-script)
- [API endpoints](#api-endpoints)
- [Reading the tunnel status](#reading-the-tunnel-status)
- [Encryption key](#encryption-key)
- [Running as a non-root user](#running-as-a-non-root-user)
- [License](#license)

## Requirements

To run a release binary: nothing. It carries the SQLite engine, the UI and
everything else it needs.

| To do this | You need |
|------------|----------|
| Run a release binary | Nothing else |
| Build from source | Go 1.23 or newer |
| Run the container | Docker and Docker Compose |

The SQLite driver is a pure Go one (`github.com/glebarez/sqlite` over
`modernc.org/sqlite`), so the binaries are built with `CGO_ENABLED=0` and need no
C library on the machine they land on.

### Platforms

`make release` builds a binary for each of these. `go build` works on anything
else Go supports, with the note below.

| Platform | Release binary |
|----------|----------------|
| Linux amd64 | `tunnel-manager-linux-amd64` |
| Linux arm64 | `tunnel-manager-linux-arm64` |
| macOS Intel | `tunnel-manager-darwin-amd64` |
| macOS Apple silicon | `tunnel-manager-darwin-arm64` |
| Windows amd64 | `tunnel-manager-windows-amd64.exe` |

On Unix the process raises its own limit on open file descriptors at startup,
because every tunnel holds several of them. Windows has no such per process
limit to raise, so that step does nothing there. Nothing else differs.

The bundled systemd unit is for Linux. On the other platforms the process has to
be kept running by whatever that system uses.

Only the Linux binaries have been run. The others are built and checked by the
compiler and the vet tool for their platform, and no more than that.

## How it works

### The reconcile loop

Tunnel Manager holds two pictures of the world and keeps comparing them.

| Picture | What it is | Where it comes from |
|---------|------------|---------------------|
| Desired | Every enabled Host combined with every service port | The `hosts` and `service_ports` rows |
| Actual | The tunnels that are running right now | The manager inside the process, and the `tunnels` rows it writes |

A **reconcile pass** compares the two and closes the gap: what is desired but not
running is started, what is running but no longer desired is stopped, and a
tunnel whose connection settings (server address, remote address, local port,
user, password) no longer match the rows is stopped and started again with the
new ones.

This is what an API request does now:

```text
POST /api/service-port
  transaction { INSERT INTO service_ports } commit
  wake the reconcile loop
  201 Created          <- the answer does not wait for any tunnel

reconcile loop
  desired = enabled Hosts x service ports
  actual  = the running tunnels
  desired but not running  -> start
  running but not desired  -> stop
  running with stale settings -> stop and start again
```

A pass runs at three moments:

| When | Why |
|------|-----|
| At startup, before the API answers anything | The tunnels of the stored rows are up by the time the first request can ask about them |
| Right after a `POST`, `PUT` or `DELETE` is committed | The change takes effect at once instead of waiting for the next tick |
| Every reconcile interval, 5 seconds by default | Anything a failed pass left undone is tried again |

Because the answer is sent before the tunnel exists, **a write that succeeds does
not mean the tunnel came up.** `GET /api/status` is what answers that. See
[Reading the tunnel status](#reading-the-tunnel-status).

### A single tunnel

```mermaid
sequenceDiagram
    participant Host as Host (SSH server)
    participant Bastion as Tunnel Manager
    participant WAS as Service

    rect rgb(255, 255, 220)
        Note over Host,WAS: Initial Setup Phase
        Bastion->>Host: SSH Authentication (Password)
    end

    rect rgb(255, 255, 220)
        Note over Host,WAS: Tunnel Creation Phase
        Bastion->>Host: Create SSH Tunnel
        Note right of Bastion: For each service port:-R 0.0.0.0:localPort:remoteIP:remotePort
    end

    rect rgb(255, 255, 220)
        Note over Host,WAS: Service Access Phase
        Host->>Host: Connect to localPort (listener bound to 0.0.0.0)
        Host->>Bastion: Forward Traffic through tunnel
        Bastion->>WAS: Forward to remoteIP:remotePort
        WAS-->>Bastion: Response
        Bastion-->>Host: Response through tunnel
    end

    Note over Host,WAS: Monitoring & Auto-reconnect
    loop Every monitoring interval
        Bastion->>Host: keepalive@tunnel check
        alt Connection Lost
            Bastion->>Host: Reconnect SSH Tunnel
        end
    end
```

Whether the listener on the Host really opens on `0.0.0.0` is up to the SSH
server on the Host. When its `GatewayPorts` is off, the listener is bound to the
loopback address whatever address was asked for, and the log says so.

The monitoring interval and the reconcile interval are two different jobs. The
monitor asks a tunnel that is already up whether it is still alive and reconnects
it when it is not. The reconcile loop asks whether the right set of tunnels
exists at all.

## Install and run

**An installation is one file.** Download the binary for your platform from the
releases page, make it executable and start it. It creates the database file, the
account and everything else it needs on the first startup.

```bash
chmod +x tunnel-manager-linux-amd64
./tunnel-manager-linux-amd64
```

The flags are all of them:

| Flag | What it does |
|------|--------------|
| `-db <path>` | The database file. It holds the settings, the registered hosts and the account, and it is created, directories above it included, if it is not there |
| `-reset-settings` | Puts every stored setting back to its default, prints what it changed and exits. See [If the server will not start](#if-the-server-will-not-start) |
| `-version` | Prints the version and exits |
| `-help` | Prints the flags and exits |

### Where the files go

**One directory holds the whole installation.** `-db` names the database file,
and everything else this installation is made of sits in the directory that file
is in.

```text
<the directory the database file is in>/
    tunnel-manager.db          the settings, the hosts, the service ports, the account
    tunnel-manager.db-wal      the write ahead log SQLite keeps beside it
    tunnel-manager.db-shm      the shared memory file SQLite keeps beside it
    initial-password           written on the first startup, deleted by the setup
    keys/tunnel-manager.key    the key the SSH passwords are encrypted with
    logs/tunnel-manager.log    the log file and the rotated files beside it
```

The key file and the log file are **settings**, not flags: they are on the
Settings screen, and their defaults are `keys/tunnel-manager.key` and
`logs/tunnel-manager.log`. **A relative path in a setting is read against the
directory the database file is in, and not against the working directory.** The
working directory is never the same twice, so a relative default read against it
would put the key somewhere different on every host. Give a setting an absolute
path and that path wins, which is how the key or the log can be put outside the
installation directory on purpose.

**Nothing is created in the directory the process was started from.**

The log is the one thing that is a file rather than a row in that database, and
for three reasons. The logger has to stand before the database is open, because
opening it is the step most likely to fail and something has to be able to say
why. The pool holds a single connection, so every log line would queue behind
the queries the process is actually there to run. And the database reports its
own statements through that logger, which would make writing a log line a query
that writes a log line.

Left out, `-db` is worked out from the place the platform keeps user data in.

| Started | `-db` | The directory the installation lives in |
|---------|-------|------------------------------------------|
| No flags, Windows | Not given | `%AppData%\tunnel-manager\` |
| No flags, macOS | Not given | `~/Library/Application Support/tunnel-manager/` |
| No flags, Linux | Not given | `$XDG_CONFIG_HOME/tunnel-manager/`, or `~/.config/tunnel-manager/` when that variable is not set |
| The bundled systemd unit | `/var/lib/tunnel-manager/tunnel-manager.db` | `/var/lib/tunnel-manager/` |
| Docker Compose | `/data/tunnel-manager.db` | `/data/`, which the compose file binds to `./_data` on the host |

A machine with neither `$XDG_CONFIG_HOME` nor `$HOME` has no such place, and the
startup says so rather than inventing one:

```text
Failed to work out where the database file goes: no default location for the database file
is available: neither $XDG_CONFIG_HOME nor $HOME are defined. Give -db an absolute path
```

The startup logs the absolute path of the database file, of the key file and of
the log file it opened, so the log always says which files are in use.

### With Docker Compose

```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
docker-compose up -d
```

The image starts the binary with `-db /data/tunnel-manager.db`, and the compose
file binds `/data` to `./_data` on the host. That is what keeps the database,
the key and the logs when the container is replaced.

The initial password file is written inside that directory, so it is readable
from the host as well:

```bash
docker compose exec tunnel-manager cat /data/initial-password
```

### As a systemd service

`_scripts/systemd/tunnel-manager.service` starts the binary with an absolute
`-db`:

```ini
ExecStart=/usr/local/bin/tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db
StateDirectory=tunnel-manager
```

`StateDirectory=tunnel-manager` makes `/var/lib/tunnel-manager` and hands it to
the account in `User=`, and the database, the key, the logs and the initial
password all sit in it. The unit ships with `User=root`; see
[Running as a non-root user](#running-as-a-non-root-user) to change that.

### From source

```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
make
./tunnel-manager
```

`make` builds the binary with `CGO_ENABLED=0`. `make release` builds one for
every platform in the table above.

## First startup and the account

**The API and the UI are behind a login.** There is one account, and it is
created on the first startup. Read this section before the first startup, or you
will not be able to log in.

1. The first startup creates the single row of the `user` table. It has **no
   username yet** and is marked as needing setup.
2. The initial password is written to a file named `initial-password` **in the
   directory the database file is in**, with permission `0600`. It is 52
   characters of upper case letters and digits.
3. **The log holds the path, never the password.** The log goes to the console
   as well as to a file that is kept and rotated, so a password written there
   would outlive the setup in places nobody is watching. The file is the only
   copy.
4. Log in with it. The username is ignored at this point, so send it empty. The
   answer carries `"setup_required": true`, and the UI moves to the setup screen.
   There you choose the username and the password the account keeps.
5. **The setup deletes the initial password file.** The password in it stops
   opening the account at that moment, so what is left would be nothing but a
   readable copy of a dead credential. Every other session that was opened with
   the initial password is dropped at the same time; the one doing the setup is
   kept.
6. **Until the setup is done, a session may call `POST /api/setup` and nothing
   else.** Every other path under `/api` answers `403` with `The account setup is
   not finished`.

The password chosen at the setup has to be **12 to 72 bytes** long. Bytes, not
characters: one Hangul syllable is three of them. The upper limit is 72 because
bcrypt, which hashes the password, reads no further than that, and anything past
the limit would not be part of what the login checks. A password that is too
long is refused rather than silently cut.

The setup runs **once**. It asks for no current password, so it is not a way to
change the credentials later; a second call answers `409`.

A session lives for 12 hours after the last request that used it, and every
request pushes that deadline out. Sessions are kept in memory, so a restart ends
all of them and you log in again.

## The built-in UI

Open `http://<address>:<port>/` in a browser. `/` answers with a redirect to
`/ui/`, which is where the UI is served from.

**There is nothing to deploy for it.** The files are compiled into the binary, so
no directory travels next to it and no path has to be configured.

| Screen | Path | What it shows and does |
|--------|------|------------------------|
| Status | `/ui/status` | The three counts (desired, rows, connected), a sentence about the difference between them, and one line per tunnel: Host, service port, status, server, local, remote, retries, last connected, last error. It asks again every 5 seconds. |
| Hosts | `/ui/hosts` | One row per Host with ID, IP, port, user, description, enabled and updated. Add a Host, edit one, enable or disable one, delete one. |
| Service Ports | `/ui/service-ports` | One row per service port with ID, service IP, service port, local port, description and updated. Add, edit and delete. |
| Logs | `/ui/logs` | The end of the log file, newest last, with a level filter and a count to show. It asks again every 5 seconds. It reads the file the process is writing now; rotated files are not shown. |
| Settings | `/ui/settings` | Every stored setting, what a save changed and whether it is in place, the certificate being served with a button to renew it and boxes to register one of your own, and the Uninstall at the bottom. See [Settings](#settings). |
| Login | `/ui/login` | Where a client without a session lands. Leave the username empty on the first sign in. It leads to the setup screen while the account still needs one. |

The version of the binary is in the bottom right corner of every screen, the
login one included.

The forms check what is typed before anything is sent. A port takes digits only
and has to be between 1 and 65535; an IP field takes only what an address is
made of and has to read as an IPv4 or an IPv6 address. What is wrong is said
next to the field it is wrong in, and nothing leaves the browser until it is
right.

The UI files are served without a session on purpose: they are the same bytes for
every client and carry no data. Everything they show is fetched from `/api/**`,
and that is what the login guards.

## Settings

**There is no configuration file.** Every setting is a column of the one
`settings` row in the database file, and the Settings screen is where it is
changed. The same values are readable and writable through `GET /api/settings`
and `PUT /api/settings`.

| On the screen | Field in the API | Reported as | Default | When it applies |
|---------------|------------------|-------------|---------|-----------------|
| API port | `api_port` | `api.port` | `8888` | At the next start |
| Serve over HTTPS | `api_https_enabled` | `api.https_enabled` | `true` | At the next start |
| Monitoring interval (seconds) | `monitoring_interval_sec` | `monitoring.interval_sec` | `5` | At the next start |
| Reconcile interval (seconds) | `reconcile_interval_sec` | `reconcile.interval_sec` | `5` | At the next start |
| Encryption key file | `security_key_file` | `security.key_file` | `keys/tunnel-manager.key` | At the next start |
| Log level | `logging_level` | `logging.level` | `info` | **The moment it is saved** |
| Log format | `logging_format` | `logging.format` | `json` | At the next start |
| Log file | `logging_file_path` | `logging.file.path` | `logs/tunnel-manager.log` | At the next start |
| Log size before rotation (MB) | `logging_file_max_size` | `logging.file.max_size` | `100` | At the next start |
| Rotated files kept | `logging_file_max_backups` | `logging.file.max_backups` | `5` | At the next start |
| Days a rotated file is kept | `logging_file_max_age` | `logging.file.max_age` | `30` | At the next start |
| Compress rotated files | `logging_file_compress` | `logging.file.compress` | `false` | At the next start |

**The log level is the one setting the running process takes on.** It reaches
every logger that was handed out at startup, the one the database writes its
statements through included, which is the half of `debug` it is usually turned on
for. Everything else is stored and read at the next start; the screen says so per
field, and the answer to a save marks each change as `now` or `restart`.

A save is refused before it is stored when a value would not hold:

| Setting | Rule |
|---------|------|
| `api_port` | 1 to 65535 |
| `monitoring_interval_sec`, `reconcile_interval_sec` | Above zero |
| `security_key_file` | Not empty |
| `logging_level` | `debug`, `info`, `warn`, `error`, `dpanic`, `panic` or `fatal` |
| `logging_format` | `json` or `console` |
| `logging_file_max_size`, `logging_file_max_backups`, `logging_file_max_age` | Zero or more |

```bash
curl -s -b cookies.txt -X PUT "$BASE/api/settings" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"api_port":9999,"logging_level":"debug"}'
```

```json
{
  "success": true,
  "data": {
    "settings": { "api_port": 9999, "logging_level": "debug", "...": "..." },
    "changes": [
      { "name": "api.port", "from": "8888", "to": "9999", "applied": "restart" },
      { "name": "logging.level", "from": "info", "to": "debug", "applied": "now" }
    ],
    "restart_required": true
  }
}
```

The body is bound onto what is stored, so a request that names some of the
settings changes those and leaves the rest alone.

### If the server will not start

A setting that keeps the process from starting used to be a file you could edit.
It is in the database now, and the screen that would change it is served by the
server that will not start. `-reset-settings` is the way out.

```bash
./tunnel-manager -db <path> -reset-settings
```

It puts every setting back to its default, prints what it changed and exits. The
next start runs on the defaults, and the Settings screen is reachable again. A
stored set that does not pass the rules above says so and names this flag:

```text
fatal  failed to read the settings  {"error": "the stored settings are refused: invalid API port: 0.
       Start with -reset-settings to put every setting back to its default"}
```

## Uninstall

The bottom of the Settings screen removes this installation. It stops every
tunnel, deletes the files the installation is made of and ends the process.
`POST /api/uninstall` is the same thing from a script.

> **Removing the encryption key cannot be undone.** The SSH password of every
> Host is sealed with that key. A backup of the database taken beforehand does
> not help: the passwords in it stay unreadable, and every Host has to be
> registered again with its password on a fresh installation.

| Removed | Left alone |
|---------|------------|
| The database file, with the `-wal` and `-shm` files SQLite keeps beside it | **The program file** |
| The encryption key file | The service entry that starts it |
| The initial password file, if it is still there | The directories the files were in |
| The log file and the rotated log files beside it | |

**The program file is not removed.** A running process cannot delete its own
image on Windows, and on Unix it would stay on disk until the process ends
anyway, which is half of a job rather than one done. Remove it by hand, along
with the systemd unit or the compose file if this was set up as a service.

The **password of the account is asked for again** and checked before anything is
touched. A session left open on an unattended screen is otherwise one press away
from this, and a password is the one thing a passer-by cannot supply. A wrong one
answers `401` and stops nothing.

The order matters and is fixed: the reconcile loop is stopped first, because it
is what starts tunnels again; then the tunnels come down, so no listener is left
behind on a remote host; then the database is closed and the files go; then the
answer is written; and the process ends about three seconds later, so the browser
has the time to receive it.

```bash
curl -s -b cookies.txt -X POST "$BASE/api/uninstall" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"password":"<your-password>"}'
```

```json
{
  "success": true,
  "data": {
    "removed": [
      { "path": "/var/lib/tunnel-manager/tunnel-manager.db", "what": "the database" },
      { "path": "/var/lib/tunnel-manager/keys/tunnel-manager.key", "what": "the encryption key" },
      { "path": "/var/lib/tunnel-manager/logs/tunnel-manager.log", "what": "the log file" }
    ],
    "failed": [],
    "exit_in_sec": 3
  }
}
```

A file that could not be removed is reported under `failed` with the reason and
is left on disk for you to deal with. It does not stop the rest: on Windows the
log file this process is writing to cannot be deleted while it is open, and a run
that stopped there would leave the database and the key behind over the one file
that was never going to go.

## Calling the API from a script

**Every path under `/api` needs a session, and every `POST`, `PUT` and `DELETE`
needs a CSRF token as well.**

1. `POST /api/login` with the username and the password. Keep the cookies it
   sets, `tm_session` and `tm_csrf`.
2. Read `data.csrf_token` out of the answer and send it as an `X-CSRF-Token`
   header on **every** `POST`, `PUT` and `DELETE`.
3. `GET` needs no token. Nothing it reaches changes anything.

CSRF stands for cross site request forgery: another site making your browser send
a request with your cookies attached. The token defeats it because that site
cannot read the answer of your login and cannot set the header.

The server sends no `Access-Control-Allow-Origin` header at all. That is a rule
browsers apply to pages, not a check this server performs, so `curl`, scripts and
server to server calls are untouched; a page on another origin is not.

The example below is a complete session. It uses a cookie jar file: `-c` writes
the cookies the server sets, `-b` sends them back.

```bash
BASE=http://127.0.0.1:8888

# 1. Log in. -c stores tm_session and tm_csrf in cookies.txt.
curl -s -c cookies.txt -X POST "$BASE/api/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"operator","password":"<your-password>"}' > login.json

# {"success":true,"data":{"setup_required":false,"csrf_token":"<token>"}}

# 2. Take the token out of the answer.
CSRF=$(python3 -c 'import json; print(json.load(open("login.json"))["data"]["csrf_token"])')

# 3. A read needs the cookies and nothing else.
curl -s -b cookies.txt "$BASE/api/status"

# 4. A write needs the header as well.
curl -s -b cookies.txt -X POST "$BASE/api/host" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"ip":"192.0.2.10","port":22,"user":"ubuntu","password":"<host-password>","description":"example"}'

curl -s -b cookies.txt -X POST "$BASE/api/service-port" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"service_ip":"198.51.100.20","service_port":8080,"local_port":18080}'

# 5. Log out when the script is done.
curl -s -b cookies.txt -X POST "$BASE/api/logout" -H "X-CSRF-Token: $CSRF"
```

On the very first sign in, log in with an empty username and the initial
password, then finish the setup before anything else. `initial-password` sits in
the directory the database file is in.

```bash
curl -s -c cookies.txt -X POST "$BASE/api/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"\",\"password\":\"$(cat initial-password)\"}" > login.json

CSRF=$(python3 -c 'import json; print(json.load(open("login.json"))["data"]["csrf_token"])')

curl -s -b cookies.txt -X POST "$BASE/api/setup" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"username":"operator","password":"<your-password>"}'
```

What goes wrong, and what it looks like:

| Answer | What it means |
|--------|---------------|
| `401 Authentication required` | No session cookie was sent, or the session has run out. Log in again. |
| `403 The request carries no valid X-CSRF-Token header...` | The write carried no token, or the wrong one. Send `data.csrf_token` from the login. |
| `403 The account setup is not finished...` | The account still has no username. Call `POST /api/setup` first. |
| `401 Invalid username or password` | The login was refused. It does not say which of the two was wrong, on purpose. |
| `400 The settings are refused: ...` | A setting broke one of the rules above. Nothing was stored. |
| `401 The password does not open this account` | The uninstall carried the wrong password. Nothing was stopped and nothing was removed. |

Every answer has the same shape: `{"success":true,"data":...}` or
`{"success":false,"error":"..."}`.

## API endpoints

Everything under `/api` requires a session, except `POST /api/login`. Everything
that is not a `GET` requires the `X-CSRF-Token` header.

### Account

| Method | Path | What it does |
|--------|------|--------------|
| `POST` | `/api/login` | Takes `username` and `password`, sets the session and CSRF cookies, answers with `setup_required` and `csrf_token` |
| `POST` | `/api/logout` | Drops the session and expires both cookies |
| `POST` | `/api/setup` | Sets the username and the password once, on the account that still needs them |

### Hosts

| Method | Path | What it does |
|--------|------|--------------|
| `POST` | `/api/host` | Creates a Host |
| `GET` | `/api/host` | Lists the Hosts |
| `GET` | `/api/host/:id` | Reads one Host |
| `PUT` | `/api/host/:id` | Updates a Host. Every field is optional; `enabled` false stops its tunnels |
| `DELETE` | `/api/host/:id` | Deletes a Host |

### Service ports

| Method | Path | What it does |
|--------|------|--------------|
| `POST` | `/api/service-port` | Creates a service port |
| `GET` | `/api/service-port` | Lists the service ports |
| `GET` | `/api/service-port/:id` | Reads one service port |
| `PUT` | `/api/service-port/:id` | Updates a service port. `service_ip`, `service_port` and `local_port` are all required |
| `DELETE` | `/api/service-port/:id` | Deletes a service port |

### Status

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/api/status` | The counts and every tunnel |
| `GET` | `/api/status/:hostId` | The Host and the tunnels of that Host |

### Settings and uninstall

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/api/settings` | The stored settings |
| `PUT` | `/api/settings` | Stores the settings in the body over the stored ones, and answers with what changed and whether a restart is needed |
| `GET` | `/api/certificate` | The certificate being served: fingerprint, subject, issuer, the names it covers, the validity and the days left |
| `POST` | `/api/certificate/renew` | Makes another self-signed certificate and serves it from the next connection on |
| `PUT` | `/api/certificate` | Takes `cert_pem` and `key_pem`, stores them and serves them from the next connection on |
| `POST` | `/api/uninstall` | Takes `password`, removes the installation and ends the process |
| `GET` | `/api/logs` | The end of the log file. `lines` says how many, up to 2000 |

The three certificate calls answer with a `409` while `api_https_enabled` is
off, because there is no certificate in use then. Neither the answer to a
replacement nor the answer to a read carries the private key: it is stored
encrypted with the same key the SSH passwords are sealed with and never leaves
the process. `cert_pem` may be a chain, with the server certificate first and
the intermediates behind it.

A replacement takes effect on the next connection and not on the ones that are
already open: TLS settles on a certificate during the handshake and the
connection never looks again. So the answer to a renewal arrives over the old
certificate, and the browser goes on showing the old fingerprint until the page
is loaded again. Both fingerprints, the old and the new, are written to the log
when a replacement happens.

The answer to `/api/logs` carries the lines and what was done to get them:
`path` is the file it read, `requested` and `max_lines` say what was asked for
and what the cap is, `capped` says whether the cap was the one that applied, and
`size` and `read` are the size of the file and how much of the end of it was
read. The file is read from the end, so `read` stays small however large the
file is.

Each line comes back split into `level`, `time`, `caller`, `message` and
`extra`, with `raw` holding the line as it was written and `parsed` saying
whether the split worked. A line that could not be split is still returned, with
`parsed` false: a line that looks wrong is the one worth reading.

### UI

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/` | Redirects to `/ui/` with a `302` |
| `GET` | `/ui` | Redirects to `/ui/` with a `302` |
| `GET` | `/ui/version.json` | The version of the binary, as `{"version":"3.0.0"}` |
| `GET` | `/ui/*` | Serves the UI out of the binary |

`/ui/version.json` is answered without a session, like the rest of `/ui/`. The
login screen shows the version too, and the number is on the release page of a
public repository either way.

The SSH password of a Host and the password hash of the account are left out of
every answer.

## Reading the tunnel status

```bash
curl -s -b cookies.txt http://127.0.0.1:8888/api/status
```

```json
{
  "success": true,
  "data": {
    "desired_tunnels": 1,
    "total_tunnels": 1,
    "connected_tunnels": 0,
    "tunnels": [
      {
        "host_id": 1,
        "sp_id": 1,
        "status": "starting",
        "last_error": "",
        "retry_count": 0,
        "last_connected_at": "0001-01-01T00:00:00Z",
        "server": "192.0.2.10:22",
        "local": "0.0.0.0:18080",
        "remote": "198.51.100.20:8080"
      }
    ]
  }
}
```

The three counts answer three different questions, and the gaps between them
mean different things.

| Count | What it counts |
|-------|----------------|
| `desired_tunnels` | How many tunnels **should** be running: enabled Hosts multiplied by service ports, counted the same way a reconcile pass builds its desired state |
| `total_tunnels` | How many tunnel **rows** exist, one per tunnel that has been started at all, whatever state it ended in |
| `connected_tunnels` | How many of those rows say `connected` |

| Gap | What it means |
|-----|---------------|
| `desired > total` | A tunnel that should be running has not been started at all. Either a pass has not run yet, which lasts a moment, or the pass could not start it, for example because the stored password does not open with the encryption key in use. The reason is in the log. |
| `total > connected` | A tunnel was started and is not carrying traffic. Its row says why in `status` and `last_error`. |

`status` is one of:

| Value | Meaning |
|-------|---------|
| `starting` | The row was written and the SSH connection is being built |
| `connected` | The listener is open on the Host |
| `reconnecting` | The connection dropped or a keepalive went unanswered, and it is being built again |
| `error` | The attempt failed. `last_error` holds the reason |

`GET /api/status/:hostId` answers with the same counts except `desired_tunnels`,
plus the Host itself.

## Encryption key

The SSH password of a Host is encrypted with AES-256-GCM before it is stored. The
key is read from the file the **Encryption key file** setting names,
`keys/tunnel-manager.key` by default. If that file does not exist, the first
startup creates a 32 byte key with permission `0600`; if it does, it is read as
it is.

> **Lose the key and no stored password can be read again.** There is nothing to
> do about it but register every Host once more. Back the key file up together
> with the database file, or the two will not match.

If the key file can be read by the group or by others, the startup refuses to go
on. Narrow it with `chmod 600` and start again.

If the key opens none of the stored passwords and at least one of them is marked
as having been encrypted, the startup stops rather than serving an API that looks
healthy while no Host can be connected to. If it opens some but not all, the ones
it does not open are named in a warning, their tunnels are not built, and the
stored values are left untouched: a password that does not open exists nowhere
else and is gone once it is written over. Set those through the API again.

A relative path in this setting is read against the directory the database file
is in, so the default puts the key in `keys/` next to the database. An absolute
path is left alone and is how the key is kept somewhere else, on a volume of its
own for instance. The startup logs the absolute path of the file it opened, so
the log says which key was read.

## Running as a non-root user

The process starts without root. It logs a `not running as root` warning and
raises what it can.

- It tries to raise the file descriptor limit to 65535. Without root the soft
  limit can only go as high as the hard limit, and when the hard limit is lower
  it logs `max ulimit is low` and goes on. Raise the hard limit in advance if you
  run many tunnels.
- The API port below 1024 cannot be bound by a non-root process. Use 1024 or
  above, or give the executable `CAP_NET_BIND_SERVICE`.
- The process has to be allowed to **write to the directory the database file is
  in**. The initial password file goes there on the first startup, and a
  directory it cannot write to stops the startup, since an account whose password
  nobody can read is an API nobody can log in to.

A `service_ports.local_port` below 1024 will not be opened by the sshd on the
Host. That listener is created by the sshd and not by Tunnel Manager, so the
restriction applies to the SSH account registered for the Host and not to the
account Tunnel Manager runs as. As ssh(1) puts it, privileged ports are forwarded
only for the root user. If the SSH account on the Host is not root, keep
`local_port` at 1024 or above.

### Ownership and the service account

There is no second directory to arrange. The key and the logs default to `keys/`
and `logs/` under the directory the database file is in, so an account that may
write that directory has everything it needs. A log file that cannot be created
does not stop the startup: file logging is turned off, the console keeps
everything, and the reason is in the `logging to file is disabled` warning.

For systemd, `_scripts/systemd/tunnel-manager.service` ships with `User=root`.
Change the account and leave `StateDirectory=` alone:

```ini
[Service]
User=tunnel-manager
Group=tunnel-manager
```

`StateDirectory=tunnel-manager` makes `/var/lib/tunnel-manager` owned by
`User=`/`Group=`, and a directory that was already there owned by root changes
owner too. An existing key file has to be readable by that account as well, so
change its owner and leave the permission at `0600`.

The container runs as root, because `Dockerfile` ends with `USER root`. To run it
as somebody else, give the service in docker-compose.yaml a `user: "<uid>:<gid>"`
and make `./_data` on the host owned by that uid. If it ever ran as root, that
directory is owned by root and has to be changed first. The file descriptor limit
comes from `ulimits` in docker-compose.yaml and has nothing to do with the
account inside the container.

## License

MIT License
