# Tunnel Manager reference

[Back to the README](../README.md)

This is everything: the install, the settings, the API and what the screens do.
The [README](../README.md) is the short version.

Tunnel Manager opens SSH tunnels and keeps them open. You register the SSH
servers it should log in to (a **Host**) and the services it should publish (a
**service port**), say which Host carries which service port, and it builds one
tunnel per assignment and watches it from then on. It is a single binary that
serves a REST API, a browser UI and the tunnels themselves.

The tunnels are **reverse** tunnels, which means the listening socket is opened
on the Host and not on the machine Tunnel Manager runs on. A client that
connects to `local_port` **on the Host** is carried through the SSH connection to
Tunnel Manager, which then connects to `service_ip:service_port` and copies the
bytes in both directions. That is how a service only Tunnel Manager can reach is
made reachable from the Host. A [local forward](#local-forwards) is the one
thing that runs the other way: the port is opened on this machine and the
connection is made from the Host.

| What you register | Fields | What it is |
|-------------------|--------|------------|
| Host | `ip`, `port`, `user`, `private_key`, `key_passphrase`, `password`, `description`, `enabled` | An SSH server Tunnel Manager logs in to. It logs in with a private key, with a password, or with both; at least one of the two is required. The key, its passphrase and the password are all stored encrypted. |
| Service port | `service_ip`, `service_port`, `local_port` | The service to publish, and the port opened on every Host that carries it. |
| Assignment | `host_id`, `sp_id`, `bind_scope` | One Host paired with one service port: this Host is to carry it. It is what a tunnel is built from, and it is made for you as a Host or a service port is registered. `bind_scope` is how far its forwarded port is asked to reach on the Host, `loopback` or `wildcard`, and the wildcard where it is not given. |
| Local forward | `local_port`, `bind_scope`, `target_ip`, `target_port`, `description` | A port opened on this machine, whose connections are carried through the SSH connection of one Host to `target_ip:target_port` as the Host sees it. It belongs to that Host alone. |

Both `ip` and `service_ip` take an IPv4 or an IPv6 address. An IPv6 address is
written plainly, as `2001:db8::1`, and the brackets a dialer needs are put on
where the address is used. A zone, as in `fe80::1%eth0`, is refused.

**One assignment whose Host is enabled is one tunnel.** A Host that carries
three service ports runs three tunnels, and a Host that carries none runs none
however many service ports are stored. Registering a Host assigns it every
service port there is, and registering a service port assigns it to every Host
there is, unless the request says otherwise, so an installation that never
touches the assignments runs every combination the way it always did. What a
Host carries after that is changed with the **Service ports** button in its row
on the Hosts screen and through
[the service ports a Host carries](#the-service-ports-a-host-carries).

**A Host is logged in to with a key, with a password, or with both.** Send
`private_key` as the text of a PEM private key file, and `key_passphrase` next to
it when the key is protected by one. The key is read as it is registered, so a
file that is not a key, a key that needs a passphrase that was not given, and a
passphrase that does not open the key are all refused there and then instead of
at the next connection. Where both are registered the key is offered first and
the password is what the connection falls back to, so putting a key on a Host
whose tunnels are running cannot take them down. A key registered while those
tunnels are up is used the next time the connection is made.

Neither the key, nor its passphrase, nor the password ever leaves the process
again: no answer carries them, and a Host that is edited comes back with those
boxes empty. An empty box leaves what is stored as it is, and a key that is sent
replaces the stored key and its passphrase together.

**There is nothing to install beside the binary.** The database is a SQLite file
the process creates itself, the settings are kept in that file and are changed on
a screen in the browser, and the UI is compiled into the executable. No database
server, no configuration file, no directory that has to travel next to it.

## Contents

- [Requirements](#requirements)
- [How it works](#how-it-works)
- [Install and run](#install-and-run)
- [HTTPS and the certificate](#https-and-the-certificate)
- [First startup and the account](#first-startup-and-the-account)
- [The built-in UI](#the-built-in-ui)
- [Settings](#settings)
- [Updates](#updates)
- [Uninstall](#uninstall)
- [Calling the API from a script](#calling-the-api-from-a-script)
- [OpenAPI and the Swagger UI](#openapi-and-the-swagger-ui)
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
| Build from source | Go 1.26 or newer |
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
| Desired | Every service port assigned to a Host, on every Host that is enabled | The `hosts`, `service_ports` and `host_service_ports` rows |
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
  desired = the assignments whose Host is enabled
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

### Assignments

**A tunnel stands for an assignment and for nothing else.** The
`host_service_ports` table holds one row per pair of a Host and a service port,
and the pair is the whole of the row, so the database itself refuses to hold the
same assignment twice.

| What happens | What becomes of the assignments |
|--------------|---------------------------------|
| A Host is registered | It is given every service port that is stored at that moment, unless the request sends `assign_all_service_ports` as false |
| A service port is registered | Every Host that is stored at that moment is given it, unless the request sends `assign_to_all_hosts` as false |
| A Host is disabled | They stay where they are. Its tunnels are stopped, and enabling it again brings them back |
| A Host or a service port is deleted | The assignments naming it go in the same transaction |
| The first startup after the upgrade that added the table | Every Host is given every service port |

The last row is what an upgrade needs. Every Host carrying every service port
was what the reconcile loop assumed before the table existed, so it was stored
nowhere, and a startup that left the new table empty would read as "no Host
carries anything" and take every tunnel down. It is filled in on the startup
that creates the table and on no other: a later startup would put back the
assignments that have since been taken away, which is the whole of what the
table is for.

An assignment naming a Host or a service port that is not there builds nothing
and is passed over by a reconcile pass. Nothing can be built from it: the
address, the port and the credentials all sit on the rows that are gone.

**How far the forwarded port reaches is part of the assignment.** `bind_scope`
on the row says which addresses on the Host the port is asked to be opened on,
and it names a pair of them rather than one.

| `bind_scope` | What is asked for on the Host | What it is for |
|--------------|-------------------------------|----------------|
| `loopback` | `127.0.0.1` and `::1` | The port is to answer on the Host itself and nowhere else |
| `wildcard`, and the empty value | `0.0.0.0` and `::` | The port is to answer wherever the Host can be reached |

**Both addresses of the pair are asked for**, because neither address family
stands in for the other: a client that connects to `::1` does not reach a port
opened on `127.0.0.1`, and the same holds for the two wildcards. Whoever picks a
scope picks it for the machine and not for one family, so both are asked for and
the tunnel row says which of them the SSH server took, see
[Reading the tunnel status](#reading-the-tunnel-status). There is no box for an
address typed in by hand: this program never asks a Host what interfaces it has,
so an address written here would be a guess, and a wrong guess is a forward that
never opens and says nothing.

The scope is held by the assignment because the Host and the service port both
have a say in it. One Host carries several service ports, and one of them may be
meant for that machine alone while the next is to be reached from elsewhere; one
service port is carried by several Hosts, and only some of them face a network
nobody else should reach in over. Either end on its own forces one answer on the
other.

An assignment stored before the column existed holds the empty value, which is
the wildcard, so an upgrade takes reach away from nothing that is running. An
installation that set a bind address on the **Host** in v3.8.3 has that answer
carried onto every assignment of that Host: a loopback address there becomes
`loopback`, and every other answer, the empty one and an address of an interface
of the machine among them, becomes the wildcard. It is carried on the startup
that adds the column and on no other, the way the assignments themselves are
filled in once, or a later startup would write over what has been chosen since.

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
        Note right of Bastion: For each assigned service port, both addresses of its bind scope: -R bindAddress:localPort:remoteIP:remotePort
    end

    rect rgb(255, 255, 220)
        Note over Host,WAS: Service Access Phase
        Host->>Host: Connect to localPort (listener bound to bindAddress)
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

**The host key of the Host is checked before any of this.** The check runs
inside the handshake, so a server that is not the one the Host is trusted on is
never offered the password or the private key, and a Host whose key nobody has
approved builds no tunnel at all. See [Host keys](#host-keys).

Which addresses are asked for is `bind_scope` on the assignment, the wildcards
where nothing was chosen, see [Assignments](#assignments). Both of them are
asked for over the one SSH connection, so **one tunnel holds two forwards**, and
a server that opens one and refuses the other leaves the tunnel carrying traffic
over half of what was asked for. The row stays one row, because one row is what
somebody chose.

Whether a listener on the Host really opens where it was asked for is up to the
SSH server on the Host. When its `GatewayPorts` is off, the listeners are bound
to the loopback addresses whatever was asked for, and when it is on they are
bound to every interface even where the loopback scope was chosen. Once the
tunnel is up, tunnel-manager tries the forwarded port itself and reports what it
found, see
[Whether the forwarded port can be reached](#whether-the-forwarded-port-can-be-reached).

The monitoring interval and the reconcile interval are two different jobs. The
monitor asks a tunnel that is already up whether it is still alive and reconnects
it when it is not. The reconcile loop asks whether the right set of tunnels
exists at all.

### Local forwards

**A local forward runs the other way from a tunnel.** A tunnel has the Host open
a port and carries what arrives there to a service this machine reaches. A local
forward has **this machine** open `local_port`, and carries every connection to
it over the SSH connection of a Host to `target_ip:target_port`, an address the
Host reaches. It is what `ssh -L` does, kept up the way a tunnel is.

```mermaid
flowchart LR
    client([A client that reaches this machine])
    subgraph here [The machine tunnel-manager runs on]
        port[["local_port<br/>opened by tunnel-manager"]]
        tm[tunnel-manager]
    end
    subgraph host [Host - an SSH server you register]
        sshd[SSH server]
    end
    target[("target_ip:target_port<br/>any address the Host can reach")]

    tm ==>|"1. connects over SSH, then opens local_port"| sshd
    client -->|"2. connects to local_port"| port
    port -->|"3. through the SSH connection"| sshd
    sshd -->|"4. connects to the target"| target
```

**A local forward belongs to one Host.** It is a row of its own, carried by the
Host it was made on and by no other, and there is no assignment for it: a
service port is shared by the Hosts that carry it, a local forward is not. It is
added, changed and deleted from the **Local forwards** button in the row of that
Host on the Hosts screen, or through the API, see
[The local forwards of a Host](#the-local-forwards-of-a-host). Deleting the Host
deletes its local forwards in the same transaction.

| Field | What it is |
|-------|------------|
| `local_port` | The port opened on this machine, 1 to 65535 |
| `bind_scope` | Which addresses of this machine it is opened on, see below |
| `target_ip` | Where a connection goes from the Host. An IPv4 or an IPv6 address, not a name |
| `target_port` | The port of the target, 1 to 65535 |
| `description` | Free text |

**`bind_scope` here is about this machine, not the Host.** It takes the same two
words an assignment does and names the same pairs of addresses.

| `bind_scope` | Where `local_port` is opened on this machine |
|--------------|----------------------------------------------|
| `wildcard`, and the empty value | `0.0.0.0` and `::`. This is the default |
| `loopback` | `127.0.0.1` and `::1` |

**The wildcard makes this machine a door into the network of the Host.** Anybody
who can reach this machine on `local_port` reaches `target_ip:target_port` as the
Host sees it without logging in to the Host, because the SSH login is the one
tunnel-manager made. Choose `loopback` unless something other than this
machine is meant to use the forward, and put a firewall in front of the port if
something is.

Both addresses of the pair are tried, each in its own address family, and **one
of them opening is enough**: a machine without IPv6 opens the IPv4 half alone.
Where neither opens, a port another program holds for instance, the forward
reports `error` and tries again.

**`local_port` is unique across every local forward**, whichever Host carries
them, because every one of them opens its port on this same machine. A second
forward on a port that is taken is refused with `409`, and so is a forward on
the port this server is stored to listen on (`api_port`) or on the port it
listens on now, when a start moved to another one. The same check is made
the other way when `api_port` changes, by a save on the Settings screen or by a
settings import: a port a forward opens is refused with `409` and nothing is
stored. The refusal carries that forward and `suggested_port`, the first port
above it that no forward, the stored `api_port`, the port listened on now and
the asked for one hold, and
the Settings screen answers it with a panel that moves the forward to another
port, or on a save the API port instead. A port below 1024 is opened by this process itself, so it is held to
the rule in [Running as a non-root user](#running-as-a-non-root-user).

**A local forward runs while its Host is enabled** and on no other Host. The
reconcile loop starts, rebuilds and stops local forwards the way it does tunnels:
one row is one SSH connection of its own, and a forward whose Host, credentials,
trusted host key, port, scope or target changed is stopped and started again. A
change to the description alone rebuilds nothing.

**`local_port` is open only while the SSH connection stands.** It is opened after
the connection is made and closed whenever the connection drops, so a client
that connects while there is no Host to carry it is refused outright rather than
accepted and dropped. The connection is checked every monitoring interval, the
way a tunnel is, and a forward whose connection dropped is connected again after
that same interval.

**A refused login and a refused host key stop it.** Trying again with the same
password or key would only fail the same way, so the forward waits until
something it is built from changes: new credentials on the Host, or the key
approved, see [Host keys](#host-keys).

`connected` says the SSH connection stands and `local_port` is open. It does not
say the target answers: every client connection is dialled from the Host on its
own, a target that cannot be reached closes that one connection, and the reason
goes to the log. The SSH server on the Host also has to allow forwarding in this
direction; OpenSSH refuses it where `AllowTcpForwarding` is `no` or `remote`.

| `status` | What it means |
|----------|---------------|
| `disabled` | The Host is disabled. Nothing is opened |
| `stopped` | The Host is enabled and no forward runs: the reconcile loop has not reached the row yet, or it failed to start it, which the log says |
| `starting` | The first connection is being made |
| `connected` | The SSH connection stands and `local_port` is open |
| `reconnecting` | The connection dropped or an attempt failed, and it is being made again. `retry_count` counts these |
| `error` | The last attempt failed, and `last_error` says why. After a refused login it stays here until the Host is changed; otherwise it is tried again after the monitoring interval |
| `host_key_unapproved`, `host_key_mismatch` | The host key was refused, as on a tunnel. It stays here until the key is approved |

**The status is kept in memory**, by the process that runs the forward, and is
answered only by the local forward calls. It is not in `GET /api/status`, and
the Status screen does not count local forwards.

**An export carries the local forwards of each Host**, in `local_forwards` on
that Host. See [Export and import](#export-and-import).

## Install and run

**An installation is one file.** Download the binary for your platform from the
releases page, make it executable and start it. It creates the database file, the
account and everything else it needs on the first startup.

```bash
chmod +x tunnel-manager-linux-amd64
./tunnel-manager-linux-amd64
```

A release carries a `SHA256SUMS` file beside the binaries, and it says what
each download should be. Releases made before that file was added carry none,
and there is then nothing to check a download against. Check it before the first start: a download that was
cut short, or served by something in the middle, still looks like a binary.
`grep tunnel-manager-linux-amd64 SHA256SUMS | sha256sum -c -` answers with the
file name and `OK`, and macOS has `shasum -a 256 -c -` in place of
`sha256sum -c -`. The `grep` is there because the file lists every platform
while only one of them was downloaded, and the names in it carry no directory,
so the check runs wherever the files were put.

The flags are all of them:

| Flag | What it does |
|------|--------------|
| `-db <path>` | The database file. It holds the settings, the registered hosts and the account, and it is created, directories above it included, if it is not there |
| `-install` | Installs this program as a service of this system and exits: the executable is put in place, the data directory is made, and the service is registered to start at boot and to come back on its own. It needs root, or an administrator on Windows. See [Installing as a service](#installing-as-a-service) |
| `-uninstall` | Stops the service, takes its registration out, removes the installed executable and exits. The data is kept. See [Removing the installation](#removing-the-installation) |
| `-bin` | Where `-install` puts the executable, and where `-uninstall` looks for one on a machine that has no registration left. Left out, it is the place this platform keeps programs an administrator installed. It goes with `-install` or with `-uninstall` |
| `-purge` | With `-uninstall`, removes the data directory as well. What it removes cannot be brought back |
| `-reset-settings` | Puts every stored setting back to its default, prints what it changed and exits. The registered hosts, the service ports, the account and the certificate are left as they are. See [If the server will not start](#if-the-server-will-not-start) |
| `-trust-proxy-headers` | Believes the `X-Forwarded-Proto` header of whatever is in front of this server, which marks the session cookies `Secure` on a connection that reaches this process in the clear. Off unless it is given. See [Behind a reverse proxy](#behind-a-reverse-proxy) |
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
`logs/tunnel-manager.log`. **Both are read against the directory the database
file is in, and not against the working directory.** The working directory is
never the same twice, so a relative default read against it would put the key
somewhere different on every host.

**Both have to stay under that directory.** A relative path is what they take,
and `logs/a/b/x.log` or `x.log` is as free as it gets; an absolute path and a
path that climbs out with `..` are refused when they are saved. The process
creates the log file and appends to it, and the Logs screen reads its tail
back, so a path that could leave the installation made the setting a way to
have this process write into any file on the machine and read any file it can
open. An installation that stored such a path while it was still accepted comes
up on the default instead and says what it replaced:

```text
warn  a stored path setting names a place outside the directory the database file is in,
      which is no longer allowed, and was put back to its default
      {"setting": "logging.file.path", "from": "/var/log/tunnel-manager/x.log",
       "to": "logs/tunnel-manager.log"}
```

**A key that was kept outside the installation directory is not read after
that.** The startup creates a new one beside the database, and the passwords
sealed with the old key do not open under it, which is what stops the startup
in [Encryption key](#encryption-key). Move the key file into the installation
directory and name it there.

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

[`-install`](#installing-as-a-service) writes a systemd unit that starts the
binary with an absolute `-db`:

```ini
ExecStart=/usr/local/bin/tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db
StateDirectory=tunnel-manager
```

`StateDirectory=tunnel-manager` makes `/var/lib/tunnel-manager` and hands it to
the account in `User=`, and the database, the key, the logs and the initial
password all sit in it. The unit is written with `User=root`; see
[Running as a non-root user](#running-as-a-non-root-user) to change that.

### Installing as a service

**`-install` makes this program a service of the machine it is run on.** It puts
the executable where this platform keeps programs an administrator installed,
makes the data directory, registers the service with the service manager of the
platform and starts it. From then on the service comes up at boot, and is started
again on its own when it exits.

```bash
sudo ./tunnel-manager-linux-amd64 -install
```

On Windows the same command is run from a PowerShell or a Command Prompt started
with **Run as administrator**. Started any other way, the install refuses and
touches nothing:

```powershell
.\tunnel-manager-windows-amd64.exe -install
```

Where things go when no path is given:

| | Linux | macOS | Windows |
|---|-------|-------|---------|
| Executable | `/usr/local/bin/tunnel-manager` | `/usr/local/bin/tunnel-manager` | `C:\Program Files\tunnel-manager\tunnel-manager.exe` |
| Data directory | `/var/lib/tunnel-manager` | `/Library/Application Support/tunnel-manager` | `C:\ProgramData\tunnel-manager` |
| Registration | a systemd unit, written over the one that is already registered or, when none is, at `/etc/systemd/system/tunnel-manager.service` | the LaunchDaemon `/Library/LaunchDaemons/io.github.jollaman999.tunnel-manager.plist` | the `tunnel-manager` service of the service control manager |
| The account it runs as | `root` | `root` | `LocalSystem` |
| Started again when it exits | `Restart=always`, 5 seconds later | `KeepAlive` | three restarts, 5 seconds apart |

`-bin` puts the executable somewhere else and `-db` puts the database somewhere
else. The database path given here is the one the registration passes to the
service, and it is the one a removal reads back out of that registration later:

```bash
sudo ./tunnel-manager-linux-amd64 -install -bin /opt/tunnel-manager/tunnel-manager \
  -db /opt/tunnel-manager/tunnel-manager.db
```

**The binary that is installed is the latest release, and not necessarily the
file that was run.** `-install` reads the newest release from GitHub and
downloads the asset for this platform and architecture. When that release carries
a `SHA256SUMS` file, the download is checked against the line for that asset, and
a checksum that does not match stops the install rather than putting the file in
place. Everything else - a release that cannot be read, a release with no asset
for this platform, a download that failed - installs the executable of the
running process instead, with the reason. Which of the two landed is in the
report:

```text
tunnel-manager install
  executable    /usr/local/bin/tunnel-manager
  taken from    the v3.5.1 release, checksum verified
  md5 before    no file was there
  md5 after     0b7f4b1c9d2e5a6f8c3d1e4b7a9f2c5d
  data          /var/lib/tunnel-manager
  database      /var/lib/tunnel-manager/tunnel-manager.db
  service       /etc/systemd/system/tunnel-manager.service
  registration  new, nothing was registered before
  state         started
```

What an install does on a machine that already carries one depends on the paths
the registration holds:

| The registration | What `-install` does |
|------------------|----------------------|
| There is none | Registers the service and starts it |
| Holds the same executable and the same database | Stops the service, writes the executable over, registers again and starts it. The md5 before and the md5 after are both in the report |
| Holds another executable, or another database | Refuses, names both, and touches nothing. Run `-uninstall` first |

**An install that would leave an older one behind is refused rather than made.**
An installation registered at other paths owns an executable and a database this
install would not touch: the new registration would name other files, and the old
ones would sit on disk with nothing on the machine naming them, which no
uninstall afterwards can find.

### Removing the installation

`-uninstall` stops the service, takes the registration out and removes the
executable that registration was started from.

```bash
sudo tunnel-manager -uninstall
```

**The data is kept**, and the report says where it was left:

```text
tunnel-manager uninstall
  taken from    the registration of this system
  executable    /usr/local/bin/tunnel-manager, removed
  data          /var/lib/tunnel-manager, left in place
  database      /var/lib/tunnel-manager/tunnel-manager.db
  service       /etc/systemd/system/tunnel-manager.service, removed
  state         stopped and taken out of the service manager
```

**Nothing has to be named on the command line.** This program keeps no state file
anywhere: the registration itself holds both the executable path and the `-db`
path, so the registration is what a removal reads.

| Platform | What the removal reads the paths out of |
|----------|------------------------------------------|
| Linux | `systemctl show tunnel-manager -p FragmentPath -p ExecStart` |
| macOS | the `ProgramArguments` of `/Library/LaunchDaemons/io.github.jollaman999.tunnel-manager.plist` |
| Windows | the `BinaryPathName` the service control manager holds for the `tunnel-manager` service |

**A machine with no registration has nothing removed.** The registration is the
only thing that says what belongs to this installation, so on a machine whose
registration is already gone, `-bin` and `-db` name what is left:

```bash
sudo tunnel-manager -uninstall -bin /usr/local/bin/tunnel-manager \
  -db /var/lib/tunnel-manager/tunnel-manager.db
```

The two flags fill in what the registration does not name, and a registration
that names a path wins over them. A removal that took the flags instead would
remove a path nothing on the machine claims and leave the registered one behind.

`-purge` removes the data directory as well.

```bash
sudo tunnel-manager -uninstall -purge
```

> **What `-purge` removes cannot be brought back.** The database, every Host and
> every credential in it, and the key the stored passwords are encrypted with,
> all go with the directory. A backup of the database taken without that key file
> does not help: the passwords in it stay unreadable. `-purge` names the
> directory on the console before removing it.

The directory `-purge` hands to a recursive removal is worked out from a `-db`
that was either typed by hand or read out of a registration this program did not
necessarily write. So anything that is not the data directory of one installation
is refused:

| `-purge` refuses | Why |
|------------------|-----|
| A directory the named database file is not in | It is then not the directory of this installation at all |
| A path that is not absolute | What it means depends on where the command was run from |
| The root of the filesystem, or a directory directly under it | `/`, `/var`, `/opt` and `C:\ProgramData` hold far more than this installation |
| `/var/lib`, `/var/log`, `/var/tmp`, `/var/cache`, `/usr/bin`, `/usr/lib`, `/usr/local`, `/usr/share`, `/etc/systemd`, `/Library/Application Support`, `/Library/LaunchDaemons`, `C:\Windows\System32`, `C:\Program Files\Common Files` | Each of them holds more than one installation |
| A home directory, which is one directory under `/home` or under `/Users` | A database file somebody kept in their home is no reason to remove everything they have |

**On Windows the executable that is running cannot be deleted.** Windows holds
the image of a running process open, and `-uninstall` is normally run from the
very executable that was installed. The file is then handed to the next boot to
be removed, and the report says `removed at the next reboot of this machine`
in place of `removed`.

**Only the Linux path has been run.** The macOS and the Windows backends are
built and checked by the compiler, by the vet tool for their platform and by unit
tests that hold the plist and the service configuration they produce. Neither has
been installed, started or removed on the system it is for.

### From source

```bash
git clone https://github.com/jollaman999/tunnel-manager.git
cd tunnel-manager
make
./tunnel-manager
```

`make` builds the binary with `CGO_ENABLED=0`. `make release` builds one for
every platform in the table above.

## HTTPS and the certificate

**The API and the UI are served over HTTPS, with a certificate this installation
makes for itself.** Open `https://<address>:<port>/` in a browser. The first
startup generates the certificate and stores it in the database file, so there
is nothing to prepare beforehand and no file to put anywhere.

**It is still one port.** A request that arrives in the clear on it is answered
with a `307` redirect to the same address under `https`, so a bookmark or a
script that still says `http://` lands where it meant to. `307` keeps the method
and the body, which `301` and `302` turn into a `GET`. The body of a request
that arrived in the clear is never read: it is on the wire as it was written, and
the client sends it again over TLS. Nothing has to be opened in a firewall that
was not open before.

| The generated certificate | |
|---------------------------|--|
| Made | On the first startup, and again when the stored one cannot be read or has run out |
| Key | ECDSA on the P-256 curve |
| Good for | 5 years |
| Made out to | `localhost`, `127.0.0.1`, `::1`, the host name of the machine and the addresses of its interfaces |
| Stored | In the database file, next to the settings. The private key is encrypted with the same key that encrypts the SSH passwords, so a copy of the database file alone does not carry it |
| Signed by | Itself |

The startup writes the fingerprint to the log, with the names the certificate
covers and the day it runs out.

### The browser warning

**Nobody signed for this certificate, so a browser warns about it and a script
refuses it.** That is what a certificate no authority issued looks like, and no
setting makes the warning go away on its own.

The fingerprint is what the warning is worth checking against. It is in the
startup log, and on the Settings screen once you are in, written the way
`openssl x509 -fingerprint -sha256` writes it: 32 bytes in upper case hex
separated by colons. Hold what the browser shows in its certificate viewer
against it. The same fingerprint means the connection is to this server; a
different one means something is answering in its place.

```bash
openssl s_client -connect 127.0.0.1:8888 </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

To be rid of the warning rather than clicking through it, take the certificate
into the trust store of the machine you browse from. The Settings screen shows
it as PEM for that, which is the same bytes the server hands to every client
during the handshake. The other way out is to register a certificate of your own.

`curl` refuses the certificate with exit code `60`. Point it at the certificate
with `--cacert <file>`, or skip the check with `-k`. The example under
[Calling the API from a script](#calling-the-api-from-a-script) sets
`CURL_CA_BUNDLE` once instead, which every `curl` in that shell then reads.

### Registering a certificate of your own

**The Settings screen has two boxes, the certificate and its private key, both
as PEM.** Paste what you were issued and press Register certificate. It is
served from the next connection on, and the process is not restarted.
`PUT /api/certificate` is the same thing from a script.

If the issuer gave you intermediates, paste them into the same box, below the
server certificate and in the order they were given. The server certificate goes
first, which is the order TLS itself requires, and the chain is stored and served
whole.

The private key is stored encrypted, the same as the generated one. It is never
shown on the screen and never sent back.

What is pasted is read before it is stored, so a mistake is answered rather than
served:

| What was pasted | What happens |
|-----------------|--------------|
| Text with no PEM block in it | Refused, naming which of the two boxes it read that way |
| The two boxes filled the other way round | Refused, and it says that is what it looks like |
| A private key that is protected by a passphrase | Refused, with the `openssl pkey` line that takes the passphrase off |
| A private key that belongs to another certificate | Refused |
| A certificate that has run out | Refused: nothing would connect to it |
| A certificate whose extended key usage leaves `serverAuth` out | Refused: every client refuses such a certificate from a server, and there would be no screen left to correct it from |
| A certificate whose validity has not started yet | **Stored**, with a warning that says from when it works. Two clocks a few minutes apart are ordinary, and refusing it would make such a certificate impossible to install at all |

A refusal stores nothing, so what is being served is still what was there.

### Renewing the certificate

**Make a new certificate** on the Settings screen replaces the one in use with
another self-signed one. It is the same generation the first startup performs, so
the new certificate is made out to the names and addresses this machine answers
to **now**, which is what a machine that was given another address needs.
`POST /api/certificate/renew` is the same thing from a script.

**No restart.** The certificate is chosen per handshake, so the next connection
is served the new one. Two things follow from a fingerprint that changed:

- The page the button was pressed on is still being served the old certificate.
  A connection that is already open keeps the one it was opened under. Load the
  page again to see the new one.
- Every browser and every script that was told to trust the old certificate warns
  once more, until the new fingerprint is trusted as well.

A replacement is written to the log with both fingerprints, the old and the new,
so a fingerprint this installation changed can be told apart from one it did not.

### Turning HTTPS off

**Serve over HTTPS** on the Settings screen, `api_https_enabled` in the API, is
what decides it. Like the other settings it is taken up at the next start.

With it off the port speaks HTTP and nothing else. There is no certificate and no
redirect, and everything the screens send travels as it was written, the password
of this account and the SSH password of a Host among it. The three certificate
calls answer `409` while it is off, because there is no certificate in use to
read or to replace.

The certificate stays in the database. Turning HTTPS on again serves the one that
was there, with the same fingerprint it had.

It can be turned off because a certificate nobody signed for gets in the way in
some places, and an operator who cannot reach the screen cannot fix anything from
it.

### Behind a reverse proxy

A proxy that terminates TLS and reaches this server in the clear leaves the
server looking at a plain HTTP connection, and the session cookies are marked
`Secure` only on a connection that is TLS. The cookies would then travel
without that flag although the browser is on HTTPS throughout.

`-trust-proxy-headers` is what says otherwise. With it, a request that carries
`X-Forwarded-Proto: https` is treated as having arrived over TLS, and the two
session cookies are marked `Secure`.

```bash
./tunnel-manager -db /var/lib/tunnel-manager/tunnel-manager.db -trust-proxy-headers
```

**It is off unless it is given, and it decides nothing else.** That header is
one any client can send, so it is worth reading only where a proxy the operator
runs is the only thing that can reach this server. A server that is exposed
directly is left as it is: turning this on there would let a client mark its
own connection as secure.

It is a flag and not a setting because it describes the deployment around this
process rather than something to change while it runs, and because a setting
would put the question on the Settings screen of an installation that has
nothing in front of it.

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

### Changing the username and the password

The Settings screen changes both, and `PUT /api/account` is the same thing from a
script. Send the current password together with whichever of the two is changing:
`username`, `new_password` or both. A value that is not sent is left as it is,
and a request that sends neither is refused, as is one that sends the value the
account already has. The new password is held to the same 12 to 72 bytes the
setup holds it to.

**The current password is required every time**, the change that only renames the
account included. That is what tells a change apart from a session left open on
an unattended screen, and it is the same reason the setup cannot be run twice.

**Every other session is signed out, this one excepted.** It happens whichever of
the two was changed, the rename included: sessions point at the account rather
than at its name, so a rename that left them alone would change what you sign in
with and leave whoever is already signed in exactly where they were. Half the
reason to change credentials is that somebody else may know them, so the rule is
one rule: the credentials changed, therefore every session but the one that
changed them is gone. The session that made the change keeps working, including
the CSRF token it already holds, so the screen that asked for it can show what
happened. The answer says how many other clients were signed out.

A wrong current password answers `401` and changes nothing. The screen asks for
the new password twice; the second copy never leaves the browser, because a
server handed the same string twice learns nothing from the second one.

```bash
curl -s -b cookies.txt -X PUT "$BASE/api/account" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"current_password":"<the password now>","username":"operator"}'
```

```json
{
  "success": true,
  "data": {
    "username": "operator",
    "username_changed": true,
    "password_changed": false,
    "sessions_ended": 1
  }
}
```

## The built-in UI

Open `https://<address>:<port>/` in a browser. `/` answers with a redirect to
`/ui/`, which is where the UI is served from. The browser warns about the
certificate the first time; see [HTTPS and the certificate](#https-and-the-certificate)
for what to check it against.

**There is nothing to deploy for it.** The files are compiled into the binary, so
no directory travels next to it and no path has to be configured.

| Screen | Path | What it shows and does |
|--------|------|------------------------|
| Status | `/ui/status` | The three counts (desired, rows, connected), a sentence about the difference between them, and one line per tunnel: Host, service port, status, server, local, remote, port reached, retries, last connected. A tunnel with something wrong carries what went wrong on a line under it, across the whole table, and a tunnel whose forwarded port was not reached carries there what to change on the SSH server it named and what else to check. A tunnel that is up carries under it what is known about the addresses of its forward, kept in three: what was asked for, what the SSH server answered, and what a connection from here confirmed. It never says a port is open. The tunnel rows come a page at a time, ten to a page to begin with, with the size and the page chosen above the table; the three counts stay counts of every tunnel and not of the page. It asks again every 5 seconds and comes back on the page being read. |
| Hosts | `/ui/hosts` | One row per Host with ID, IP, port, user, description, enabled and updated. The rows come a page at a time, ten to a page to begin with, with the size (10, 20, 30, 50 or 100) and the page chosen above the table. The choice is remembered for this screen on its own, and a list short enough to fit a page of the smallest size carries no controls at all. Add a Host, edit one, enable or disable one, delete one. The add and edit forms have a box to paste a private key into, an area to drop the key file onto, and a box for the passphrase of a key that has one, and the add form has an **Assign all service ports** tick, on by default, that says what the Host starts out carrying, with a **Reach on the Host** list beside it that every assignment that tick makes starts on. **Service ports** in a row opens a panel of every service port with a tick against the ones this Host carries, and a reach beside each row: pick a reach above and apply it to everything ticked, or set one row on its own, and a row that was not ticked is left alone. Only what was changed is sent when it is saved, so a tick made there leaves the pages that were not read alone. **Local forwards** in a row opens a panel of the local forwards of that Host with the status of each, where they are added, changed and deleted; see [Local forwards](#local-forwards). |
| Service Ports | `/ui/service-ports` | One row per service port with ID, service IP, service port, local port, description and updated. The rows come a page at a time the same way the Hosts do, with a size and a page of their own. Add, edit and delete. The add form has an **Assign to all hosts** tick, on by default, that says which Hosts carry it from the start, with a **Reach on the Host** list beside it that the assignments that tick makes start on; which Hosts carry it after that, and what each of those assignments reaches, is changed from the Hosts screen. |
| Logs | `/ui/logs` | The end of the log file, newest last, with a level filter and a count to show. It asks again every 5 seconds. It reads the file the process is writing now; rotated files are not shown. The lines are shown in the language of the screen while the file stays English; see [The language of the screens](#the-language-of-the-screens). |
| Settings | `/ui/settings` | What is stored but not being run on yet, with a Restart in that card that puts it into place, every stored setting and what a save changed, among them the language this installation shows a browser that has picked none, the certificate being served with a button to renew it and boxes to register one of your own, the username and the password of this account, an export of the tunnel configuration and of the settings of this manager into one encrypted file each and an import that takes such a file back, a Restart that takes the service down and brings it back, and the Uninstall at the bottom. See [Settings](#settings). |
| Update | `/ui/update` | What this installation is running beside what the newest release is, and the two settings that decide whether either is looked at again. The reading is taken on a timer rather than when the screen is drawn, so opening it costs the release API nothing; a press takes it now. Where the release is newer and this process is what a service registration starts, a press installs it, which takes the password of the account and ends with the service restarting. See [Updates](#updates). |
| Manual | `/ui/manual` | What an installation is made of, drawn and said on one screen: what this does, one tunnel end to end, Hosts and service ports and the assignments between them, what an unreached port means, the two intervals, and where the files go. It asks the server for nothing, which is what lets the login screen show the same thing. |
| Login | `/ui/login` | Where a client without a session lands. Leave the username empty on the first sign in. It leads to the setup screen while the account still needs one. A **Manual** button opens the manual as a panel over it, without a session, because the state it is most needed in is the one where nothing works yet. |

The version of the binary is in the bottom right corner of every screen, the
login one included.

**The screens come in a light theme and a dark one**, and the switch is in the
top right corner of every screen, the login one included. Until it is pressed
the browser decides, and a browser that is changed from light to dark while a
screen is open is followed without the page being loaded again. A press is kept
in the local storage of that browser under `tm_theme` and is never sent
anywhere: which theme a screen is read in belongs to the screen and not to the
installation, and two people reading the same server may want different ones.

**The screens come in thirteen languages**, and the switch is the list in the
top right corner of every screen, beside the theme switch, the login one
included. Each language is listed under its own name: English, 한국어, 日本語,
中文, Español, Français, Deutsch, Português (Brasil), Русский, العربية, हिन्दी,
Tiếng Việt and ไทย. Arabic is written right to left, and the whole page turns
round with it. Which language a browser that has picked none is shown is a
setting of the installation; see
[The language of the screens](#the-language-of-the-screens) for that and for
the order in which the language is settled.

The forms check what is typed before anything is sent. A port takes digits only
and has to be between 1 and 65535; an IP field takes only what an address is
made of and has to read as an IPv4 or an IPv6 address. What is wrong is said
next to the field it is wrong in, and nothing leaves the browser until it is
right.

**A key file that is dropped is read in the browser.** What is sent is the text
of the key, exactly as a paste would be; the file itself is not uploaded, and a
file larger than a private key ever is, or anything dropped that is not a file,
is refused with a line under the drop area. The key can be pasted instead, which
is what a key that is in a terminal somewhere else wants.

The UI files are served without a session on purpose: they are the same bytes for
every client and carry no data. Everything they show is fetched from `/api/**`,
and that is what the login guards.

### The language of the screens

The language a screen is drawn in is settled in this order, and the first
answer that is there wins:

1. **What was picked in the corner of this browser.** The pick is kept in the
   local storage of that browser under `tm_lang`, as the theme is, and is never
   sent anywhere.
2. **What the installation was set to show**: the `ui_default_language`
   setting, on the Settings screen. It is what everybody who has said nothing
   is shown, and it is read once there is a session.
3. **What the browser asks for**, matched against the thirteen. A tag is
   matched whole first, so a browser set to `pt-BR` gets the Brazilian catalog,
   and then by its language alone, so one set to `pt-PT` gets that same catalog
   rather than English.
4. English.

A pick in the corner wins over the setting on purpose. The setting is what an
installation shows to somebody who has said nothing about it, and somebody who
has picked a language has said something, on the very screen they are reading.

**The login screen does not follow the setting.** Reading a setting needs a
session and the login screen has none, so it is drawn in what this browser
picked or, failing that, in what the browser asks for. That is how it is meant
to work and not something to report: the setting takes hold the moment the
login succeeds, without the page being loaded again, and again the moment it is
saved.

The codes are `en`, `ko`, `ja`, `zh`, `es`, `fr`, `de`, `pt-BR`, `ru`, `ar`,
`hi`, `vi` and `th`, and they are what `ui_default_language` takes; see
[Settings](#settings).

**What is translated is everything the screens say**, what the server answers
included. A refusal comes with a code in `error_code` and the values of its
sentence in `error_args`, and the screen draws the sentence for that code in
its own language, while `error` stays the English sentence it always was. A log
line comes with a `log_id`, and the Logs screen draws the sentence for that
identifier out of the fields of the line. **The log file itself stays
English.** It is what a `grep` runs over and what a support request carries,
and a file that followed the screen would be written in whatever language was
picked last. See [Calling the API from a script](#calling-the-api-from-a-script)
for the shape of a refusal and [Settings and uninstall](#settings-and-uninstall)
for the shape of a log line.

Each language is one catalog, served out of the binary as
`/ui/lang/<code>.json`, so an installation on a host that reaches nothing else
still has all thirteen. Every catalog carries the same keys, one per sentence
the screens can show, and a key a language has no words for is drawn in English
rather than left blank. The page fetches the catalog of the language in use and
the English one, and nothing else.

**Adding a language** is one catalog and four lists of codes, and a test holds
the five together, so a code added to one place and not to the others fails
the build rather than the page:

| Where | What |
|-------|------|
| `internal/web/static/lang/<code>.json` | The catalog, with every key of `en.json` |
| `internal/web/static/app.js`, `languages` | The code, the name the language calls itself, and whether it runs right to left |
| `internal/web/static/index.html`, `codes` and `rightToLeft` | The same two lists, read in the head before `app.js` is fetched |
| `internal/settings/settings.go`, `uiLanguages` | What `ui_default_language` is held against |
| `internal/web/web_test.go`, `catalogCodes` | The list the tests compare the other four with |

**What the translations do not do yet.** A count comes in two forms, one and
many, which is how English works and not how Russian or Arabic does, and each
catalog is worded to get by with the two. In Arabic a path that begins with `/`
can be drawn with that slash away from the rest of it: it is how a browser lays
left to right text into a right to left line, and the path itself is whole. And
none of the catalogs has been read by a native speaker yet, so a sentence that
reads oddly is worth a report.

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
| Log size before it is rotated (MB) | `logging_file_max_size` | `logging.file.max_size` | `100` | At the next start |
| Rotated log files kept | `logging_file_max_backups` | `logging.file.max_backups` | `5` | At the next start |
| Days a rotated log file is kept | `logging_file_max_age` | `logging.file.max_age` | `30` | At the next start |
| Compress rotated log files | `logging_file_compress` | `logging.file.compress` | `true` | At the next start |
| Language this installation is shown in | `ui_default_language` | `ui.default_language` | empty, which names none | **The moment it is saved** |
| Look for a newer release | `update_check_enabled` | `update.check_enabled` | `true` | **The moment it is saved** |
| How often to look (hours) | `update_check_interval_hours` | `update.check_interval_hours` | `24` | **The moment it is saved** |
| Install a newer release on its own | `update_auto_install` | `update.auto_install` | `false` | **The moment it is saved** |

**The log level, the language and the three update settings take hold as they
are saved.** The level reaches every logger that was handed out at startup, the
one the database writes its statements through included, which is the half of
`debug` it is usually turned on for. The language is never read by this process
at all: the browser reads it, out of the answer to the save that stored it and
out of every read after that, so there is nothing a restart could put into
place. An empty language is a value and not a gap: it says this installation
names none, and a browser is shown what it asks for, which is what every browser
was shown before the setting existed. Everything else is stored and read at the
next start; the screen says so per field, and the answer to a save marks each
change as `now` or `restart`.

A save is refused before it is stored when a value would not hold:

| Setting | Rule |
|---------|------|
| `api_port` | 1 to 65535. A new port a local forward opens is refused with `409`, see [Local forwards](#local-forwards) |
| `monitoring_interval_sec`, `reconcile_interval_sec` | Above zero |
| `security_key_file`, `logging_file_path` | Not empty, and a path under the directory the database file is in: an absolute path and one that climbs out with `..` are refused. See [Where the files go](#where-the-files-go) |
| `logging_level` | `debug`, `info`, `warn`, `error`, `dpanic`, `panic` or `fatal` |
| `logging_format` | `json` or `console` |
| `logging_file_max_size`, `logging_file_max_backups`, `logging_file_max_age` | Zero or more |
| `ui_default_language` | Empty, or one of `en`, `ko`, `ja`, `zh`, `es`, `fr`, `de`, `pt-BR`, `ru`, `ar`, `hi`, `vi` and `th`, written exactly so: `EN` and `ko-KR` are refused |
| `update_check_interval_hours` | 1 to 8760. Zero is refused rather than read as off, because a zero would be a timer rearming as fast as it can against an API that counts requests; `update_check_enabled` is what turns it off |

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

A read answers with the stored settings and, beside them, the ones this process
is not running on:

```bash
curl -s -b cookies.txt "$BASE/api/settings"
```

```json
{
  "success": true,
  "data": {
    "api_port": 9999,
    "logging_level": "debug",
    "...": "...",
    "pending_restart": [
      { "name": "api.port", "running": "8888", "stored": "9999" }
    ]
  }
}
```

`pending_restart` is what the server works out on every read, by holding the
settings it read when it started against the ones that are stored: `running` is
the value this process is on and `stored` is the one a restart would bring it
to. It is the same for every client and for every session, and a restart empties
it without anything being cleared, since the process comes back running on what
is stored. Neither the log level nor the language is ever in it, because both
are in place the moment they are saved. An installation that is running on
everything it has stored gets `[]`.

**A stored `api_port` that another program holds does not stop the start.** The
process listens on a port the system picks instead, one no local forward opens,
and logs it under `api_server.port_taken_fallback` with `stored_port` and `port`.
On Windows a stored port the system keeps for itself, such as one in a range
reserved for Hyper-V or WinNAT, is refused with an access error and passed over
the same way.
A restart tries the port it was running on first, before one the system picks,
and logs `reused_previous` as `true` when it lands there.
That port is not stored: the next start tries the stored one again, and until
then `pending_restart` lists `api.port` with the port in use as `running`. Where
Docker publishes the port or a firewall opens it by number, the port picked
instead may not be reachable from outside, so free the stored port and restart.

### Restarting the service

The Settings screen has a **Restart** button, and `POST /api/restart` is the same
thing from a script. It is what puts a stored setting that waits for a start into
place. The API stops answering, every tunnel comes down and is built again on the
way back, so everything going through a tunnel is cut for as long as the restart
takes. The password of the account is not asked for, unlike the uninstall below:
nothing here is final.

The answer is written first and the process goes about three seconds later, which
is the time the browser has to draw the screen that says the service is coming
back. Then the shutdown runs in the order a signal runs it in: the API server is
drained, the redirect server after it, the port is released, the reconcile loop
is stopped and the tunnels come down. Only once all of that has ended does the
process run the program again in place of itself, with the same arguments and the
same environment.

**It is the same process.** Unix replaces the image of a running process rather
than starting another one, so the PID does not change, systemd and Docker see
nothing happen, nothing is started twice and there is no second instance to fight
over the port. The port is released before the image is replaced, because the
program that replaces it binds that same port a moment later. Replacing the
binary on disk and then restarting is what runs the new one: the file is read at
that moment.

**Windows has no exec.** There the restart is an ordered stop and nothing else,
and starting the program again is left to whatever supervises the service; one
that was started by hand does not come back. Both answers carry `comes_back` so
that the screen can say which of the two this installation is before anything is
pressed, and `GET /api/restart` answers that without doing anything.

The sessions are held in memory, so they go with the process. The screen asks for
the login again once the service is back.

```bash
curl -s -b cookies.txt -X POST "$BASE/api/restart" \
  -H "X-CSRF-Token: $CSRF"
```

```json
{
  "success": true,
  "data": {
    "exit_in_sec": 3,
    "comes_back": true
  }
}
```

### If the server will not start

A setting that keeps the process from starting used to be a file you could edit.
It is in the database now, and the screen that would change it is served by the
server that will not start. `-reset-settings` is the way out.

```bash
./tunnel-manager -db <path> -reset-settings
```

It puts every setting back to its default, prints what it changed and exits. The
next start runs on the defaults, and the Settings screen is reachable again. A
local forward that opens the default API port is named in a warning
(`settings.default_port_held_by_forward`), since the next start may listen on
another port.

**Only the settings go back.** The registered hosts, the service ports, the
account and the certificate are in the same database file and are left as they
are: nothing has to be registered again, and you log in with the password you
already have.

A stored set that does not pass the rules above says so and names this flag:

```text
fatal  failed to read the settings  {"error": "the stored settings are refused: invalid API port: 0.
       Start with -reset-settings to put every setting back to its default"}
```

A stored API port that another program holds does not keep the process from
starting: it starts on another port and names it in the log, see
[Settings](#settings). Any other failure to open the port, one below 1024
without the privilege for it for instance, still ends the start.

## Updates

The Update screen says what this installation is running and what the newest
release is, and it can install that release.

**What is read is the release page of this repository, and nothing else.** The
request carries no credentials, because the repository is public; a request that
needed them would mean the release is not reachable the way an operator's
machine reaches it.

### Looking

The reading is taken on a timer, not when the screen is opened. Two people
opening the screen make no requests at all, and what they see is what the last
look found, with the time it was taken. The **Look now** button takes it again.

`update_check_enabled` turns the timer off and `update_check_interval_hours`
says how often it fires. Both take hold as they are saved: an interval changed
from a day to an hour does not wait out the day that was already running.

**A check that failed is not the same as being up to date**, and the screen says
which of the two it is. A failure is kept and drawn as a failure rather than
leaving the last good answer standing.

**A tag that does not read as three numbers is never treated as newer.** The
comparison takes `v3.8.1` and `3.8.1` and nothing else: a tag with a suffix, a
tag with four parts and a tag that is a word all answer "cannot tell", which the
screen says and which never starts an install. An answer of newer is what
replaces the executable of a running service, so a version this does not
understand is not grounds for one.

### Installing

The press is offered where the release is newer and where this process is what a
service registration starts. It takes the password of the account, the way the
uninstall does: it replaces the executable and ends with a restart that drops
every tunnel.

**It is the same work `-install` does**, because it is `-install`: the program is
run again as a process of its own, with the flag. A process cannot replace its
own file and then run itself again, so the work is handed to one that has not.
The release is downloaded, checked against the `SHA256SUMS` the release
publishes, put in place, and the service manager restarts the service.

**The answer says the install started and never that it finished.** The process
that would report the end is the one being restarted. The screen says so and
says to load the page again once the service is back, where the version in the
corner is what went through.

Where this program is running as something somebody started rather than as a
registered service, the press is not offered at all: `-install` would register
one, and nothing would start the process again afterwards. The screen says that
in place of the button, and `POST /api/update/install` answers `409`.

### Installing without being asked

`update_auto_install` is off unless it is turned on. With it on, a release that
is read as newer is installed with nobody pressing anything.

**What it does when it acts is take the service down.** Every tunnel comes down
and is built again, at whatever hour the release appears. Whether that is
acceptable depends on what runs over those tunnels and who is relying on them at
the time, which is not something this program can work out, which is why the
default leaves it with the operator.

It is subject to everything above: a check that failed, a tag that cannot be
compared and a release that is not newer all leave it alone, and an installation
that is not a registered service never reaches it.

**An exported configuration carries this setting.** A file exported from an
installation that has it on turns it on wherever the file is taken in; see
[Export and import](#export-and-import).

## Uninstall

The bottom of the Settings screen removes this installation. It stops every
tunnel, deletes the files the installation is made of and ends the process.
`POST /api/uninstall` is the same thing from a script.

> **Removing the encryption key cannot be undone.** The SSH password of every
> Host is encrypted with that key. A backup of the database taken beforehand does
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
anyway, which is half of a job rather than one done. `-uninstall` is what
removes the executable and the registration of a service installed with
`-install`; a compose file is removed by hand.

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
   sets. Over HTTPS they are named `__Host-tm_session` and `__Host-tm_csrf`;
   over plain HTTP they are `tm_session` and `tm_csrf`. The `__Host-` prefix is
   what tells a browser that the cookie belongs to this host alone, and a
   browser only takes that name from a cookie marked `Secure`, so an install
   with HTTPS turned off is served the names without it. A cookie jar keeps
   whichever pair arrived, so a script that uses one does not have to know
   which.
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
BASE=https://127.0.0.1:8888

# The certificate is the self-signed one, so curl is told where to find it. The
# Settings screen shows it as PEM; -k on every call skips the check instead.
# See [HTTPS and the certificate](#https-and-the-certificate).
export CURL_CA_BUNDLE=tm-cert.pem

# 1. Log in. -c stores both cookies in cookies.txt, under the names they came
# back with.
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
`{"success":false,"error":"..."}`. An answer that says no carries two fields
more: `error_code` names the refusal, and `error_args` holds the values its
sentence was written with, under the names the sentence uses, and is left out
when there are none. `error` is the English sentence whatever language the
screen is in, so a script that has been reading it goes on working; the code is
what the screen translates, and what a script should match on rather than the
words.

```json
{
  "success": false,
  "error": "No such service port: 99",
  "error_code": "assignment.service_port.not_found",
  "error_args": { "ids": "99" }
}
```

## OpenAPI and the Swagger UI

**Open `https://<this server>:8888/ui/api-docs/` in a browser.** Every call in
the tables below is on that page, with the fields it takes and the answers it
gives, and the **Try it out** button on each of them sends a real request to
this server.

The page comes out of the binary, the way the screens do. Nothing is fetched
from the network to draw it, so it comes up on a machine that reaches this
server and nowhere else. It needs no session of its own: what it shows is the
same for every installation and is in the reference you are reading. It is the
calls it sends that need one.

**Those calls are the real thing.** `POST /api/uninstall` on that page removes
the installation and `DELETE /api/host/{id}` deletes the Host. There is nothing
practising behind the button.

### Logging in on that page

There is no single token to paste and be done with. This API is a session cookie
and a CSRF header, so it takes two steps.

1. Find `POST /api/login`, press **Try it out**, put your username and password
   into the body and press **Execute**. The browser keeps the cookies the answer
   sets, exactly as it would after the login screen, and the answer shows
   `data.csrf_token`.
2. Copy that token, press **Authorize** at the top of the page and paste it in.
   It goes out as an `X-CSRF-Token` header from then on.

Step 1 is enough for every `GET`: the cookie is all a read needs and the browser
sends it without being asked. Step 2 is what a `POST`, `PUT` or `DELETE` needs,
and without it those come back `403` with `auth.csrf.refused`, which is the same
refusal a script gets for the same reason. See
[Calling the API from a script](#calling-the-api-from-a-script).

A browser already logged in to the screens is logged in here too: same origin,
same cookie. The token still has to go into **Authorize** by hand, and step 1 is
the shortest way to a fresh one. It is also in the `tm_csrf` cookie
(`__Host-tm_csrf` over HTTPS), which is why that cookie is not marked
`HttpOnly`, while the session cookie is and no page can read it.

### The description file

The page is drawn from a description of every path under `/api`. It is OpenAPI
2.0, generated from the source, committed, and built into the binary, so these
two are the same file:

| Where | What to read |
|-------|--------------|
| In the repository | `internal/web/static/openapi.json` |
| From a running server | `https://<this server>:8888/ui/openapi.json` |

```bash
curl -sk https://127.0.0.1:8888/ui/openapi.json > openapi.json
```

That one needs no session either. A test in this repository fails when a route
is registered that the description does not carry, and when the description
carries one no route answers, so the two do not drift apart quietly.

### Putting it into a tool

**Postman**: *Import*, then the file. Every call arrives as a request in a
collection. **Insomnia**: *Import From*, then *File*. Neither of them carries
the login. Call `POST /api/login` first and the cookies stay in the client from
then on; for a write, add the `X-CSRF-Token` header with the token that login
answered with. The description declares that header as an API key named
`CSRFToken`, so a tool that reads security definitions gives you a field to put
it in.

**Generating a client**: `openapi-generator` reads the file as it stands.

```bash
openapi-generator-cli generate -i openapi.json -g python -o ./client
```

What comes out knows the paths, the bodies and the answers. It does not know
that the session lives in a cookie, so turn on the cookie jar of whatever HTTP
library it was generated against, or every call after the login is answered
`401`.

## API endpoints

Everything under `/api` requires a session, except `POST /api/login`. Everything
that is not a `GET` requires the `X-CSRF-Token` header.

### Paging

`GET /api/host`, `GET /api/service-port` and `GET /api/status` answer one page
at a time. A list that answers with everything grows with the installation: the
answer, the memory it is built in and the screen that holds it grow with the
number of rows, none of which is looked at at once.

| Parameter | Default | What it takes |
|-----------|---------|---------------|
| `page` | `1` | The page, counted from 1. Below 1 is read as 1, and a page past the last one is answered with the **last page** rather than refused |
| `size` | `10` | How many rows a page holds. One of `10`, `20`, `30`, `50` and `100`; anything else is refused with `400` |

A page past the end is not an error because rows are deleted while a screen is
open: the page a client sits on can be gone by the time it asks again, and an
error there would leave that screen empty where the rows that are left belong. A
list with nothing stored is page 1 with an empty `items`.

`size` is taken from that list rather than as any number, because a size a
request can pick freely is a way to ask for every row in one answer, which is
what the paging is here to prevent.

**`/api/host` and `/api/service-port` changed shape.** `data` used to be the
array of rows and is now an object carrying the page:

```json
{
  "success": true,
  "data": {
    "items": [ "..." ],
    "total": 25,
    "page": 2,
    "size": 10
  }
}
```

`items` is the page, `total` is how many rows there are in all, and `page` and
`size` are what was answered, which is not always what was asked for. A client
reading `data[0]` reads `data.items[0]` now.

`/api/status` was an object already. `tunnels` is one page of the tunnel rows
now and `page` and `size` stand beside it, while **the three counts are over
every row and not over the page**: they say what the installation is doing, not
what is on the page being looked at.

```bash
# The second page of twenty Hosts, and the last page of the tunnel rows: a page
# past the end comes back as the last one, so a large number asks for it.
curl -s -b cookies.txt "$BASE/api/host?page=2&size=20"
curl -s -b cookies.txt "$BASE/api/status?page=99999&size=10"
```

### Account

| Method | Path | What it does |
|--------|------|--------------|
| `POST` | `/api/login` | Takes `username` and `password`, sets the session and CSRF cookies, answers with `setup_required` and `csrf_token` |
| `POST` | `/api/logout` | Drops the session and expires both cookies |
| `GET` | `/api/setup` | Says whether the account still needs its username and password. Answers without a session, for the login screen |
| `POST` | `/api/setup` | Sets the username and the password once, on the account that still needs them |
| `GET` | `/api/account` | What the account is called |
| `PUT` | `/api/account` | Takes `current_password` and `username`, `new_password` or both, changes them and signs out every other session |

### Hosts

| Method | Path | What it does |
|--------|------|--------------|
| `POST` | `/api/host` | Creates a Host. `enabled` is optional and a Host that does not say is enabled, and `bind_scope` is what the assignments this request makes are opened to |
| `GET` | `/api/host` | One page of the Hosts, oldest first. Takes `page` and `size`, see [Paging](#paging) |
| `GET` | `/api/host/:id` | Reads one Host |
| `PUT` | `/api/host/:id` | Updates a Host. Every field is optional; `enabled` false stops its tunnels |
| `DELETE` | `/api/host/:id` | Deletes a Host, the assignments naming it and its local forwards |
| `GET` | `/api/host/:id/service-port` | One page of the service ports with the assignments of this Host laid over them, see [The service ports a Host carries](#the-service-ports-a-host-carries) |
| `PUT` | `/api/host/:id/service-port` | Adds and removes assignments of this Host |
| `GET` | `/api/host/:id/local-forward` | The local forwards of this Host with their status, see [The local forwards of a Host](#the-local-forwards-of-a-host) |
| `POST` | `/api/host/:id/local-forward` | Adds a local forward to this Host |

The body of a create and of an update takes these fields.

| Field | On create | On update |
|-------|-----------|-----------|
| `ip`, `port`, `user` | Required | Optional; what is left out stays as it is |
| `private_key` | The text of a PEM private key file. Optional if a `password` is given | An empty or missing value keeps the stored key. A key that is sent replaces the stored key and its passphrase together |
| `key_passphrase` | Required only for a key that is protected by one | Sent with the key it belongs to. On its own, without `private_key`, it is refused |
| `password` | Optional if a `private_key` is given | An empty or missing value keeps the stored password |
| `bind_scope` | Optional. `loopback` or `wildcard`, and left out or sent empty it is the wildcard | Not read. A Host holds no scope; the scope of one assignment is changed through `PUT /api/host/:id/service-port` |
| `description`, `enabled` | Optional | Optional |
| `assign_all_service_ports` | Optional. Left out, and the Host is given every service port that is stored. Send it as false to register a Host that carries none | Not read. What a Host carries is changed through `PUT /api/host/:id/service-port` |

A create that carries neither a key nor a password is refused, and so is a key
that cannot be used. The refusal says which of them it is: that the value is not
PEM, that the key is protected by a passphrase that was not sent, or that the
passphrase does not open the key. Nothing is stored in any of those cases.

**No answer ever carries `private_key`, `key_passphrase` or `password`**, this
one included. What was stored is confirmed by the Host connecting, which the
status says.

**`bind_scope` is what the assignments this one request makes are opened to**,
the batch of them, and it is read nowhere else. A Host is registered before
anything has been said about its service ports one at a time, so the one answer
given here is what the whole batch starts on. Where the request sends
`assign_all_service_ports` as false no assignment is made and the field decides
nothing. It takes `loopback` or `wildcard` and nothing else, a third word is
refused with `400`, and left out it is the wildcard, which is what every forward
asked for before any of this existed.

The Host itself holds no scope, so `PUT /api/host/:id` does not take the field
and no answer carries it. What one assignment is opened to afterwards is read
and changed through
[the service ports a Host carries](#the-service-ports-a-host-carries), and what
the two scopes mean is in [Assignments](#assignments).

Moving an assignment to another scope rebuilds the tunnel of that assignment
alone, because the scope is part of what a tunnel is built from. The other
assignments of the same Host are left running.

```bash
# A Host that is logged in to with a key. The key is sent as the text of the
# file, so the newlines in it have to survive: this reads the file with jq.
jq -n --arg key "$(cat ~/.ssh/id_ed25519)" \
  '{ip:"192.0.2.10",port:22,user:"ubuntu",private_key:$key,description:"example"}' |
curl -s -b cookies.txt -X POST "$BASE/api/host" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  --data-binary @-
```

### The service ports a Host carries

**`GET /api/host/:id/service-port` answers one page of the service ports with
`assigned` on each of them**, saying whether this Host carries it. The page is
taken over the service ports and not over the assignments, and ordered by id the
way `GET /api/service-port` is, so a row sits on the same page of both lists
whether the Host carries it or not. It takes `page` and `size`, see
[Paging](#paging). A Host that is not there is answered with `404` rather than
with every service port and nothing assigned, which is what a Host carrying
none looks like.

```json
{
  "success": true,
  "data": {
    "items": [
      { "id": 1, "service_ip": "198.51.100.20", "service_port": 8080,
        "local_port": 18080, "description": "", "assigned": true,
        "bind_scope": "wildcard" }
    ],
    "total": 1,
    "page": 1,
    "size": 10
  }
}
```

`bind_scope` on a row is what the assignment of this Host is opened to. It is
**empty on a row this Host does not carry**: the scope is held by the assignment,
so a service port that is not assigned has none, and the row the screen would
make starts on the wildcard until something else is chosen.

**`PUT /api/host/:id/service-port` takes a change and not the whole set.** The
list is served a page at a time, so a client holds one page and knows nothing of
the rows on the pages it has not read; a whole set sent from there would name
that page alone, and every assignment outside it would be deleted by a request
meant to tick one box.

```bash
curl -s -b cookies.txt -X PUT "$BASE/api/host/1/service-port" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"add":[1,3],"remove":[2],"rescope":[1],"bind_scope":"loopback"}'
```

```json
{ "success": true, "data": { "added": 2, "removed": 1, "rescoped": 1 } }
```

| Field | What it does |
|-------|--------------|
| `add` | The service ports to give this Host. A new assignment is written on `bind_scope` |
| `remove` | The service ports to take away from it |
| `rescope` | The assignments to move to `bind_scope`, named the same way. An id this Host does not carry moves nothing |
| `bind_scope` | `loopback` or `wildcard`, and left out it is the wildcard. It is what the rows in `add` are written on and what the rows in `rescope` are moved to |

**An assignment that is already there is left on the scope it holds unless
`rescope` names it.** That is why the two are separate lists. Read the other way,
a client that ticks a box which was ticked already, or one written before any of
this existed, would widen to the wildcard an assignment somebody had pinned to
the loopback, and reach handed out by a request that did not mention it is the
one thing this must not do. `add` widens nothing; the bulk apply on the screen
fills `rescope` from what was ticked, so a row you did not tick is left alone.

`added`, `removed` and `rescoped` count the rows and not the request: a service
port that is already assigned is asked for again without a row being written,
one that is not assigned is removed without a row going, and an id in `rescope`
that names an assignment this Host does not carry moves nothing. All three lists
are optional and a request that changes nothing is answered, not refused.

| What was sent | What happens |
|---------------|--------------|
| The same service port in `add` and in `remove` | `400`, naming it. Which of the two would win is a guess at what the request meant, and what it decides is whether a tunnel runs |
| A service port id that is not stored | `400`, naming it. Nothing is written |
| A `bind_scope` that is neither `loopback` nor `wildcard` | `400`, naming the field. Nothing is written |
| A Host id that is not stored | `404` |

The whole of the change lands or none of it does, and the reconcile loop is
woken once it is committed, so the tunnels follow within the moment.

### Host keys

**A Host reaches nothing until the key of its SSH server has been approved.**
The check runs inside the handshake, before any authentication, so a server that
fails it is never offered the SSH password or the private key of the Host.
Whatever it presented is written down so that there is something to compare and
to approve, and it is never written down as trusted: nothing this end can see
makes a key the right one, and the only thing that does is a person who read the
fingerprint off the server itself.

Every answer that carries a Host carries `host_key_fingerprint`, the key that
Host is trusted on, and `pending_host_key_fingerprint`, the key some server
presented on a connection that was refused. Both are the SHA256 fingerprint and
never the key itself: the fingerprint is what `ssh` prints on a first connection
and what `ssh-keygen -lf` reports for the host key file on the server, so it is
the one form of the key that can be compared against the machine itself. A Host
that carries no trusted key yet is empty in the first, and one with nothing
waiting is empty in the second.

A Host that is waiting is in one of two states, and they are not the same
question. Which of the two it is is on the tunnel rows of that Host, in
`status`, and on the list below, in `mismatch`.

| State | What it is |
|-------|------------|
| `host_key_unapproved` | The Host carries no trusted key. It is where every Host starts, and where an upgrade leaves every Host that was registered before this check existed |
| `host_key_mismatch` | The Host is trusted on one key and was presented another. Either the server was rebuilt and given a new key, or the connection is not reaching that server at all, and the two cannot be told apart from here |

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/api/host-key` | One page of the Hosts that have a key waiting, across every Host. Takes `page` and `size`, see [Paging](#paging) |
| `POST` | `/api/host/:id/host-key` | Approves the key waiting on one Host |
| `POST` | `/api/host-key` | Approves the keys of several Hosts in one request |

A row of the list carries the comparison and nothing else of the Host: a panel
holds a hundred of them at a time, and the port, the user and the description
are a hundred rows of what nobody is reading there.

```json
{
  "success": true,
  "data": {
    "items": [
      { "host_id": 1, "ip": "192.0.2.10", "mismatch": true,
        "fingerprint": "SHA256:<the key the server presented>",
        "trusted_fingerprint": "SHA256:<the key this Host is trusted on>" }
    ],
    "total": 1,
    "page": 1,
    "size": 10
  }
}
```

`trusted_fingerprint` is empty on a Host that has never been approved, and
`mismatch` is false there. `mismatch` is carried rather than left to a client
comparing that field with the empty string, because it is what decides whether
the approval asks for the password of the account.

```bash
# Approve the key waiting on one Host. The fingerprint goes back with the
# approval, so a key the SSH server presented after the list was read is not
# approved by a press meant for the one on the screen.
curl -s -b cookies.txt -X POST "$BASE/api/host/1/host-key" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"fingerprint":"SHA256:<the fingerprint on the screen>","password":"<the account password>"}'
```

| Field | What it takes |
|-------|---------------|
| `fingerprint` | Required. The fingerprint that was compared against the server. The approval goes through only while that is still the key the row holds |
| `password` | The password of **this account**, not of the Host. Required where the Host already carries a trusted key, and read nowhere else |

**A Host that carries no key yet is approved on the session alone**, because
there is no trust to overturn and because that press is on the path of
registering every Host. Replacing a trusted key is the other thing: without the
password, a session left open on an unattended screen would be one press away
from trusting whatever is answering in place of the server.

`POST /api/host-key` is the same approval given over a list, and it is there for
what an upgrade looks like: every Host that was registered before this check
existed is waiting for a first approval at the same moment. Every Host in the
request carries its own fingerprint, since the fingerprint of one says nothing
about another, and the password is asked for once for the whole request rather
than once per Host.

```bash
curl -s -b cookies.txt -X POST "$BASE/api/host-key" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"hosts":[{"host_id":2,"fingerprint":"SHA256:<the fingerprint on the screen>"}]}'
```

```json
{
  "success": true,
  "data": {
    "approved": 1,
    "refused": 0,
    "hosts": [
      { "host_id": 2, "approved": true,
        "fingerprint": "SHA256:<what this Host is trusted on from here on>" }
    ]
  }
}
```

**A Host that is refused does not take the rest of the list down with it.** It
comes back in `hosts` with `approved` false and the refusal on it: `error`,
`error_code`, and `error_args` where the sentence has values in it, the way an
answer that says no carries one. The rest of the list goes through. One request may name **1000 Hosts** at most: every
Host of a request is read and written inside a single transaction, and the
database is run on one connection, so the length of the list is the length of
time nothing else is served.

| What was sent | What happens |
|---------------|--------------|
| A fingerprint that is not the one waiting | `409`, naming the one that is waiting. Nothing is written, and on the bulk approval it is that Host's refusal alone |
| No password, or a wrong one, where a trusted key is being replaced | `401`. Nothing is written, on any Host of the request |
| A Host that has no key waiting | `409`. Either it has been approved already, or nothing has connected to it since the last one was |
| A Host id that is not stored | `404` on the single approval, and that Host's refusal alone on the bulk one |
| More than 1000 Hosts in one request | `400`. Nothing is read |

An approval wakes the reconcile loop, so the tunnels of that Host are built
again within the moment rather than at the next pass. The Status screen carries
one line over the table while anything is waiting, drawn from the two counts in
`GET /api/status`, and the press on it opens this list.

### The local forwards of a Host

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/api/host/:id/local-forward` | Every local forward of this Host with what it reports, ordered by id. Not paged |
| `POST` | `/api/host/:id/local-forward` | Adds a local forward to this Host |
| `GET` | `/api/local-forward/:id` | Reads one local forward |
| `PUT` | `/api/local-forward/:id` | Updates a local forward. The Host it belongs to is not changed |
| `DELETE` | `/api/local-forward/:id` | Deletes a local forward |

What a local forward is and what its status means are in
[Local forwards](#local-forwards).

```bash
curl -s -b cookies.txt -X POST "$BASE/api/host/1/local-forward" \
  -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"local_port":15432,"bind_scope":"loopback","target_ip":"198.51.100.30","target_port":5432,"description":"database"}'
```

```json
{
  "success": true,
  "data": [
    { "id": 1, "host_id": 1, "bind_scope": "loopback", "local_port": 15432,
      "target_ip": "198.51.100.30", "target_port": 5432, "description": "database",
      "status": "connected", "last_error": "", "retry_count": 0,
      "last_connected_at": "<when the connection was made>",
      "created_at": "<...>", "updated_at": "<...>" }
  ]
}
```

That is what `GET /api/host/1/local-forward` answers afterwards. A create, a
read and an update answer with one such object. `bind_scope` in an answer is
always `loopback` or `wildcard`, never empty.

The body of a create and of an update takes the same fields, and an update
takes the whole of them: `local_port`, `target_ip` and `target_port` are
required on both, and **an update that leaves `bind_scope` out puts the forward
on the wildcard**, so send the scope it is on to keep it there.

The answer to a write is made before the reconcile loop has reached the row, so
the status in it can be from before the forward was started or rebuilt. Read the
list again to see what it did.

| What was sent | What happens |
|---------------|--------------|
| A port outside 1 to 65535, a `target_ip` that is not an IP address, or a `bind_scope` that is neither `loopback` nor `wildcard` | `400`. Nothing is written |
| A `local_port` another local forward opens | `409`, naming the port |
| A `local_port` that is the port this server is stored to listen on | `409`, naming the port |
| A Host id or a local forward id that is not stored | `404` |

### Service ports

| Method | Path | What it does |
|--------|------|--------------|
| `POST` | `/api/service-port` | Creates a service port. `assign_to_all_hosts` is optional: left out, every stored Host is given it, and sent as false it is registered carried by none. `bind_scope` is what the assignments it makes are opened to |
| `GET` | `/api/service-port` | One page of the service ports, oldest first. Takes `page` and `size`, see [Paging](#paging) |
| `GET` | `/api/service-port/:id` | Reads one service port |
| `PUT` | `/api/service-port/:id` | Updates a service port. `service_ip`, `service_port` and `local_port` are all required |
| `DELETE` | `/api/service-port/:id` | Deletes a service port, and the assignments naming it |

`bind_scope` on the create is what the batch of assignments `assign_to_all_hosts`
makes is opened to, the way it is on `POST /api/host`, and
`PUT /api/service-port/:id` does not take it. One answer can stand for a batch
that reaches every Host because the two words mean the same on every machine,
which is the other reason there is no box for an address typed in: an address
names an interface of one machine, and the rest would be asked to open a port on
an address they do not have.

### Status

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/api/status` | The counts of the installation and one page of the tunnel rows. Takes `page` and `size`, see [Paging](#paging) |
| `GET` | `/api/status/:hostId` | The Host and the tunnels of that Host. Not paged: a Host holds one tunnel per service port it carries |

### Settings and uninstall

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/api/settings` | The stored settings, and in `pending_restart` the ones this process is not running on |
| `PUT` | `/api/settings` | Stores the settings in the body over the stored ones, and answers with what changed and whether a restart is needed |
| `GET` | `/api/certificate` | The certificate being served: fingerprint, subject, issuer, the names it covers, the validity and the days left |
| `POST` | `/api/certificate/renew` | Makes another self-signed certificate and serves it from the next connection on |
| `PUT` | `/api/certificate` | Takes `cert_pem` and `key_pem`, stores them and serves them from the next connection on |
| `GET` | `/api/restart` | What a restart would do here: how long before the service goes and whether it comes back on its own |
| `POST` | `/api/restart` | Takes the service down in order and runs the program again in place of this process, where the platform has exec |
| `POST` | `/api/uninstall` | Takes `password`, removes the installation and ends the process |
| `GET` | `/api/logs` | The end of the log file. `lines` says how many, up to 2000 |
| `POST` | `/api/logs/clear` | Takes `password`, empties the file the log is being written to and leaves the rotated files beside it alone |
| `GET` | `/api/update` | What the last look found: the version running, the newest release, whether it is newer, and whether an install can be started from here |
| `POST` | `/api/update/check` | Reads the newest release now and answers what `GET /api/update` would then answer |
| `POST` | `/api/update/install` | Takes `password` and starts the install. It answers that the install started and never that it finished: what the install ends with is a restart of the service answering the request |

The three certificate calls answer with a `409` while `api_https_enabled` is
off, because there is no certificate in use then. Neither the answer to a
replacement nor the answer to a read carries the private key: it is stored
encrypted with the same key that encrypts the SSH passwords and never leaves
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

`message` is the English sentence the file holds, and among the fields in
`extra` is `log_id`, the name of the line, as `tunnel.connected`: it is what the
Logs screen looks up to show the line in its own language, with the values taken
from the other fields of the same line. A line written by a version that had no
names, or by something else that wrote into the file, carries no `log_id` and is
shown as it was written.

### Export and import

| Method | Path | What it does |
|--------|------|--------------|
| `POST` | `/api/export/tunnels` | Takes `password`, answers with every Host, its local forwards included, and every service port encrypted into one file |
| `POST` | `/api/import/tunnels` | Takes `password`, `file` and `overwrite`, and writes what the file holds |
| `POST` | `/api/export/settings` | Takes `password`, answers with the stored settings encrypted into one file |
| `POST` | `/api/import/settings` | Takes `password` and `file`, and stores the settings the file holds |

These four carry a configuration from one installation to another. An export
hands out a file and an import takes one back, so you decide where the file is
kept and for how long, and neither installation has to reach the other.

**What an export hands out is one line of text.** It opens with the marker
`tmpwenc:v1:` and everything after it is base64, so the whole file is ASCII and
survives being pasted into a box, a message or a ticket without anything being
lost to a line ending. Encoded in there are the parameters the key was derived
with, the salt, the nonce and the encrypted configuration. The parameters and
the salt are authenticated rather than encrypted, which is what lets a wrong
password be told apart from a damaged file, and nothing readable is in the file
at all. That is why the import screen takes a single long line, and why dropping
the file onto it and pasting what is in it do the same thing.

**The file says which service ports each Host carries**, in
`assigned_local_ports` on the Host, named by local port and not by the id of the
row: the ids belong to the installation the file came from, while the local port
is unique across the service ports of an installation and so means the same on
both sides. The list is what the Host carries after the import and not what to
add to it, so a Host the import wrote is left carrying what the file names and
nothing besides; a Host that was skipped keeps the assignments it has. A local
port the file names and this installation does not hold is listed in the answer
as a skipped assignment, with the Host and the port, and the rest of the import
stands.

A file written before the assignments were stored carries no such field, and
every Host in it is taken to carry every service port in it. That is what such a
file meant without saying it, and read as "carries none" it would import
cleanly and leave the installation with no tunnel at all. A Host that carries
nothing is written into the file as `[]`, which is the same distinction from the
other side.

**How far each assignment reaches travels with it**, in `assigned_bind_scopes`
on the Host, keyed by the same local port. An assignment with no entry there is
on the wildcard, which is what the empty value means in the database, so only an
answer other than that one is written. A file from v3.8.3 carries no such field
and one `bind_address` for the whole Host instead: that is read, and a loopback
address there makes every assignment of that Host `loopback` while every other
answer makes them the wildcard, by the same rule the upgrade uses. It is read
and never written, so an import cannot quietly undo a narrowing somebody made,
and a file this version writes carries no `bind_address` at all.

**The local forwards of each Host travel with it**, in `local_forwards` on the
Host, and `local_forwards` in the answer of the export counts them. Like
`assigned_local_ports`, the list is what the Host carries after the import, and
a missing field and an empty list are two different answers.

| `local_forwards` on a Host in the file | What an import that writes that Host does |
|----------------------------------------|-------------------------------------------|
| Missing, or `null` | Leaves the local forwards of that Host as they are. A file written before local forwards existed says nothing about them |
| `[]` | Deletes every local forward of that Host |
| A list | Replaces the local forwards of that Host with the list |

A Host that was skipped keeps its local forwards whatever the file says. The
export always writes the list, `[]` for a Host with none. Every local forward
the import writes is listed in the answer as `added` or `replaced`. **The import
is refused, and nothing is written**, when the file opens one `local_port`
twice, when a forward in it does not pass the rules of a create, when it opens
the port this server is stored to listen on, or when it opens a port that a local
forward the import leaves in place holds here; the Host of that forward is named
in the refusal.

**The key each Host is trusted on travels with it**, in `host_key`, so moving a
configuration does not throw the trust away and the installation that takes the
file in connects without every Host being approved again. It is a public key and
is written as the row holds it rather than encrypted. The key some server
presented on a refused connection is in no file, and an import that replaces a
row drops the one that was there: it is a question about a machine the other
installation has not spoken to yet, and the file has just said what the right
key is.

**The whole file is encrypted with the password given to the export, and that
password is the only thing protecting it.** Inside it, the SSH password, the
private key and the key passphrase of every Host are written in the clear. That
is what the file is for: the database keeps those encrypted with the encryption
key of the machine they were stored on, that key never leaves it, and a file
carrying them in that form could be read on no other installation. They are
decrypted on the way out and encrypted again with the key of the installation
that takes them in. So treat an exported file as the credentials of every Host
it names.

The exports are `POST` and not `GET` because the password is in the body. In a
URL it would be written to the access log of this server and to the history of
the browser that asked. The password is held to the same 12 to 72 bytes the
account password is, and it is stored nowhere: a file whose password is
forgotten cannot be opened by anyone, this program included.

An import adds what is not registered here and **skips** what is, naming in the
answer what it skipped and why. Send the same file again with `overwrite` set to
true to replace those rows instead; a replaced row keeps its id, so the tunnels
of that Host reconnect rather than being built anew. Every row of the file is
listed in the answer as `added`, `replaced` or `skipped`, which is what you read
before deciding about an overwrite. The whole import is one transaction: a file
that is refused half way through leaves the database exactly as it was. The
tunnels themselves are not carried, since the reconcile loop builds them from
the Hosts, the service ports and the assignments between them. A `reason` or a `name` that
is an English sentence comes with `reason_code` and `reason_values`, or `name_code` and
`name_values`, the code and the values a screen says it from in its own language.

The settings import **stores** the settings and puts none of them onto the
running process, `api_port` and `api_https_enabled` included. What is stored is
what the next startup runs on, and `GET /api/settings` reports the difference in
`pending_restart` until then, so an import cannot move the port out from under
the request that carries it. A file whose settings do not pass the rules of the
Settings screen is refused, and nothing is stored; so is a file whose `api_port`
is a new port a local forward opens here, with `409` as a save is.

A file that does not open says which of the four it is: the password is wrong,
it is not a file this program wrote, it is damaged, or it holds the other kind.

```bash
# Export, and keep the file where you keep secrets.
curl -s -b cookies.txt -X POST "$BASE/api/export/tunnels" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"password":"<the password that encrypts the file>"}' |
jq -r '.data.file' > tunnels.tmexport

# Import it on the other installation.
jq -n --arg file "$(cat tunnels.tmexport)" \
  '{password:"<the same password>",file:$file,overwrite:false}' |
curl -s -b cookies.txt -X POST "$BASE/api/import/tunnels" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  --data-binary @-
```

### UI

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/` | Redirects to `/ui/` with a `302` |
| `GET` | `/ui` | Redirects to `/ui/` with a `302` |
| `GET` | `/ui/version.json` | The version of the binary, as `{"version":"3.0.0"}` |
| `GET` | `/ui/lang/<code>.json` | The catalog of one language, `en` to `th`; see [The language of the screens](#the-language-of-the-screens) |
| `GET` | `/ui/openapi.json` | The OpenAPI description of `/api`; see [OpenAPI and the Swagger UI](#openapi-and-the-swagger-ui) |
| `GET` | `/ui/api-docs/` | The Swagger UI, drawn from that description |
| `GET` | `/ui/*` | Serves the UI out of the binary |

`/ui/version.json` is answered without a session, like the rest of `/ui/`. The
login screen shows the version too, and the number is on the release page of a
public repository either way.

The SSH password of a Host and the password hash of the account are left out of
every answer.

## Reading the tunnel status

```bash
curl -s -b cookies.txt https://127.0.0.1:8888/api/status
```

```json
{
  "success": true,
  "data": {
    "desired_tunnels": 1,
    "total_tunnels": 1,
    "connected_tunnels": 0,
    "host_keys_unapproved": 0,
    "host_keys_mismatched": 0,
    "page": 1,
    "size": 10,
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
        "remote": "198.51.100.20:8080",
        "server_banner": "",
        "forward_reach": "unknown",
        "error_kind": "",
        "open_reach": "",
        "listen_addresses": ""
      }
    ]
  }
}
```

`tunnels` is one page of the rows, ordered by Host and then by service port, and
`page` and `size` say which page of which size it is. The three counts are over
every row: an installation of twenty-five tunnels reports twenty-five on a page
of ten, and `connected_tunnels` counts the connected tunnels of the
installation and not the ones that happen to be on the page. See
[Paging](#paging).

The three counts answer three different questions, and the gaps between them
mean different things.

| Count | What it counts |
|-------|----------------|
| `desired_tunnels` | How many tunnels **should** be running: the assignments whose Host is enabled, counted the same way a reconcile pass builds its desired state |
| `total_tunnels` | How many tunnel **rows** exist, one per tunnel that has been started at all, whatever state it ended in |
| `connected_tunnels` | How many of those rows say `connected` |

| Gap | What it means |
|-----|---------------|
| `desired > total` | A tunnel that should be running has not been started at all. Either a pass has not run yet, which lasts a moment, or the pass could not start it, for example because the stored password does not open with the encryption key in use. The reason is in the log. |
| `total > connected` | A tunnel was started and is not carrying traffic. Its row says why in `status` and `last_error`. |

**Two more counts stand beside them, and they are over Hosts and not over
tunnels.**

| Count | What it counts |
|-------|----------------|
| `host_keys_unapproved` | Hosts whose SSH server presented a host key and that carry no approved key yet |
| `host_keys_mismatched` | Hosts that are trusted on one key and were presented another |

They are over the whole installation and not over the page, for the reason the
other three are, and for one more: a Host that carries four service ports is
refused on the same key four times, so a count over the page would carry the
same question four times over. What is waiting is the two added together, and
the list behind them is `GET /api/host-key`; see [Host keys](#host-keys).

`status` is one of:

| Value | Meaning |
|-------|---------|
| `starting` | The row was written and the SSH connection is being built |
| `connected` | The listener is open on the Host |
| `reconnecting` | The connection dropped or a keepalive went unanswered, and it is being built again |
| `error` | The attempt failed. `last_error` holds the reason |
| `host_key_unapproved` | The SSH server presented a host key and none has been approved for this Host, so the connection was refused. See [Host keys](#host-keys) |
| `host_key_mismatch` | The SSH server presented a key other than the one this Host is trusted on, so the connection was refused. See [Host keys](#host-keys) |

`error_kind` names what sort of failure `last_error` is, for the one sort there
is somewhere to send you: `forward_denied` is the SSH server refusing to open
the forwarded port. Every other failure, and every row that is not in error,
leave it empty. Like `forward_reach` it says what happened and never why, since
the several settings that make a server refuse look identical from here.

### Whether the forwarded port can be reached

A tunnel that says `connected` is one the SSH connection stands for. It does not
mean the forwarded port can be reached: the listeners are opened by the **SSH
server**, and which addresses it binds is that server's decision, not this one's.
tunnel-manager asks for both addresses of the bind scope of the assignment, the
wildcards `0.0.0.0` and `::` unless the loopback scope was chosen.

**What the SSH server answers to a request is not a measurement of what it
bound.** The reply to a forward request carries a port and no address, and there
is nothing on the far side this program asks. What decides the binding is
`GatewayPorts` in `sshd_config` for OpenSSH, and the `-a` flag on the command
line for Dropbear. This is what was measured against OpenSSH:

| `GatewayPorts` | Asked for | What the server bound | The second request |
|----------------|-----------|-----------------------|--------------------|
| `clientspecified` | `0.0.0.0` | `0.0.0.0` | `::` was opened |
| `yes` | `0.0.0.0` | `0.0.0.0` and `::` | Refused |
| `yes` | `127.0.0.1` | `0.0.0.0` and `::`, so the scope that was chosen was ignored | Refused |
| `no` | `0.0.0.0` | `127.0.0.1` and `::1`, so the scope that was chosen was ignored | Refused |

`man 5 sshd_config` has `yes` "force remote port forwardings to bind to the
wildcard address" and `no` "force ... available to the local host only". **Both
of them force, so the client is not asked**, and `clientspecified` is the one
setting under which the scope that was picked is the scope that is bound. A
refusal is not a closed port either: a server that bound both families on the
first request refuses the second, and so does a server with no IPv6 at all, and
the two are the same answer seen from here.

Four fields on every tunnel row say what is known about it.

| Field | What it holds |
|-------|---------------|
| `server_banner` | What the SSH server called itself on the handshake, for example `SSH-2.0-OpenSSH_10.5p1 Ubuntu-1ubuntu2`. It is what says which server is in front of you, and what to change on it differs by server |
| `open_reach` | Which of the two requests the SSH server said yes to: `both`, `ipv4` where it took the IPv4 address and refused the IPv6 one, `ipv6` the other way round, and empty where nothing has been measured yet. A connection on which it refused both is a tunnel that failed rather than one that reaches half, and it is reported on `status` and `last_error` like every other failure |
| `forward_reach` | Whether tunnel-manager reached the forwarded port by opening a TCP connection to the Host at that port: `reachable`, `unreachable`, or `unknown` where nothing has been measured and where nothing can be |
| `listen_addresses` | The addresses the Host itself answered that the port is listening at, comma separated, for example `0.0.0.0,::`. It is asked over the same SSH connection once the forwards are open, and it is the only reading that survives a server ignoring the scope: a Host with `GatewayPorts yes` says no to the second request and still answers here with both families. **Empty means the question was not answered, never that nothing is listening.** An account with no shell, a Host with neither `ss` nor a `netstat` this program reads, and a connection on which nothing was asked yet all leave it empty |

All four are taken once when the tunnel comes up and again on every reconnect,
not
on every status read: what decides them is the configuration of the SSH server,
which does not change under a connection that stands.

**The status screen keeps three things apart, and so should you.**

| What is said | What it rests on |
|--------------|------------------|
| What was **asked for** | The two addresses of the scope on the assignment. This end chose them, so they are known |
| What was **confirmed** | An address that answered a TCP connection opened from here, which is `forward_reach` reading `reachable`. It is the one piece of evidence this end holds |
| What is **not known** | Everything else, `open_reach` included. A yes to a request is not a port that is up, and a no is not a port that is down |

It never says a port is open. A port that answered a connection from here is
open at the address that was dialled and that is all that can be said.

> **`unreachable` says where the port was not reached from, not why.** A server
> that bound the port to loopback alone and a firewall that drops the connection
> on the way look exactly the same from here, and a connection that never
> arrives cannot tell them apart. Check both before changing either.

**An assignment on the `loopback` scope can never be confirmed from here, and
that is not a fault.** Its ports are asked for on the addresses of the Host
itself, which nothing outside that machine reaches, so no connection opened here
could ever arrive however well the port is carrying traffic over there. The
silence is written down as `unknown` rather than as `unreachable` for that
reason: a tunnel doing exactly what was asked of it must not be drawn as one
that failed. The same holds for an address family the server refused, and for
the IPv6 half of any Host this installation knows by an IPv4 address, which is
the only address of a Host it holds.

`GET /api/status/:hostId` answers with `total_tunnels` and `connected_tunnels`
over that Host, plus the Host itself. It carries neither `desired_tunnels` nor
the two host key counts. It is not paged and carries every tunnel of that Host.

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

The path in this setting is read against the directory the database file is in,
so the default puts the key in `keys/` next to the database. It has to stay
under that directory: an absolute path is refused, and one that was stored
while it was still accepted is put back to the default at the next startup. The
startup logs the absolute path of the file it opened, so the log says which key
was read. See [Where the files go](#where-the-files-go).

## Running as a non-root user

The process starts without root. It logs a `not running as root` warning and
raises what it can.

- It tries to raise the file descriptor limit to 65535. Without root the soft
  limit can only go as high as the hard limit, and when the hard limit is lower
  it logs `max ulimit is low` and goes on. Raise the hard limit in advance if you
  run many tunnels.
- The API port below 1024 cannot be bound by a non-root process. Use 1024 or
  above, or give the executable `CAP_NET_BIND_SERVICE`. The same holds for the
  `local_port` of a local forward, which this process opens itself.
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

For systemd, the unit `-install` writes has `User=root`. Change the account and
leave `StateDirectory=` alone:

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
