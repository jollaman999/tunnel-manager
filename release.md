# v3.0.0

## Breaking changes:

**An installation from an earlier release cannot be carried over.** There is no upgrade path and no migration tool: the storage, the configuration and the flags all changed. Register the hosts and the service ports again on a fresh installation.

- There is no database server. The data lives in a SQLite file the binary opens itself, so MySQL and MariaDB are no longer needed or used.
- **There is no configuration file.** `-config` is gone. The one thing the binary has to be told is where the database file is, and that is `-db`. Left out, it falls where the platform keeps user data.
- Every other setting moved into the database and is changed on the Settings screen of the web UI. A configuration file that still carries the old sections is refused by name rather than ignored.
- The initial password file is written next to the database file rather than next to the configuration file.
- A relative `security.key_file` or `logging.file.path` is read against the directory the database is in, not against the working directory. The same setting therefore lands in the same place however the process was started.
- The compose file no longer runs a database container, and it mounts one directory rather than three.

## Add/fix features:

- One binary, one directory:
  - Running the binary with no arguments puts the database, the encryption key, the log and the initial password under the directory the platform keeps user data in. Running it from an unrelated directory leaves nothing behind there.
  - The bundled systemd unit and the compose file name the path outright, so a service does not inherit one from whatever HOME it happens to run with.
- Settings screen:
  - Every stored setting is edited from the web UI. A save answers with what changed and whether each change is running now or waiting for the next start, and the screen prints that rather than deciding it.
  - The log level takes hold at once, gorm's statement logging included. It used to be fixed when the database was opened, so asking for debug turned on everything except the statements, which are the reason to ask.
  - A setting that would stop the next start is refused at the point of saving, since there is no longer a file to correct it in. `-reset-settings` puts everything back for whatever gets past that.
- Logs screen:
  - The end of the log file is readable from the UI, with a level filter and the same five second refresh the status screen uses. The file is read from the end, so a large log costs neither time nor memory.
  - A line that cannot be parsed is shown as it was written rather than dropped.
  - The log stays a file. The logger has to stand before the database is open, the connection pool is held to one connection, and gorm logs the queries, so a log in the database would be missing exactly when it is needed.
- Uninstall:
  - The Settings screen can remove the installation: it stops the tunnels, stops the reconcile loop, closes the database and takes the database with its -wal and -shm, the encryption key, the log with its rotated copies and the initial password. The program file is left where it is.
  - It asks for the login password again, so a screen left open cannot do this with one press, and it says plainly that a backup of the database cannot be read once the key is gone.
- The name of the program stays in view after the sign in rather than only on the way in.

## Notes:

- SQLite takes one writer at a time, so the writers are put in a queue by holding the pool to a single connection. A mutex around the handlers would not have reached the reconcile loop, which writes from the tunnel package.
- The driver is pure Go, so the release binaries still cross compile for five platforms with CGO off.

# v2.1.0

## Add/fix features:

- IPv6:
  - A Host or a service port may be given an IPv6 address. The API took one all along, since the validator behind the `ip` tag accepts it, and then built `2001:db8::1:22` out of the address and the port. Nothing could take that apart, because the port cannot be told from the last group of the address. The addresses are joined with `net.JoinHostPort` now, which writes `[2001:db8::1]:22`.
  - An address carrying a zone, as in `fe80::1%eth0`, is refused. The validator has always refused it and the UI now says so before the request is sent.
- Built-in UI:
  - The version of the binary is shown in the bottom right corner of every screen, the login one included. It is read from the new `GET /ui/version.json`, which is answered without a session so that the login screen can show it too.
  - The forms check what is typed before anything is sent. A port takes digits only and has to be between 1 and 65535. An IP field takes only what an address is made of and has to read as an IPv4 or an IPv6 address. What is wrong is said next to the field it is wrong in, and the request is not sent until it is right.
  - The dates and the row buttons no longer wrap onto a second line. The timestamps carry seconds, which the status screen needs: it asks again every five seconds, and without them two rows written seconds apart read as the same moment.
  - The tunnel state is a badge rather than a word, numeric columns line up on the right, and Delete no longer looks like the button beside it.

## Bug fixes:

- An IPv6 address accepted by the API produced a tunnel address no dialer could parse. See above.

# v2.0.2

## Bug fixes:

- Built-in UI:
  - On a phone held upright the page scrolled sideways and took the heading and the navigation off screen with it. The tables carry more columns than that width fits, and nothing held them inside the page. Each table now sits in a box that scrolls on its own, so only the table moves.
  - Below 34rem the forms put the label above the input instead of beside it. Two fixed columns added up to more than the screen is wide, which pushed every input past the right edge.
  - The three counts on the status screen share the rows two at a time rather than running off the end. "Connected" was cut off before.
  - Wider screens are unchanged. The new rules are inside a media query and nothing above it was touched.

## Documentation:

- `release.md` holds the notes of every release back to v0.0.1. It said it did and held the last two.

# v2.0.1

## Bug fixes:

- Encryption key:
  - A fresh install started by the bundled systemd unit wrote its encryption key to `/keys/tunnel-manager.key`, in the root of the filesystem. `security.key_file` is relative by default and a relative path is read against the working directory, which systemd leaves at `/`. The unit now declares a state directory and works from it, so the default resolves under `/var/lib/tunnel-manager`.
  - The default itself is unchanged. The container relies on it together with `WORKDIR /` to land on the volume the compose file maps to `./_data/keys`.
  - An installation already running with an explicit absolute `security.key_file` is unaffected. One that took the default under a unit of its own should check where its key actually is before upgrading, and is best given an absolute path.
- Startup log:
  - The path the encryption key was read from is logged as an absolute path rather than as the value that was configured. `keys/tunnel-manager.key` said nothing about which file was opened, which is how the key at `/` went unnoticed.

## Documentation:

- The README says where the default key file lands for each way of starting the process, and that an absolute `security.key_file` makes that table irrelevant.

# v2.0.0

## Breaking changes:

This release changes how the API is called. A client written against v1.0.0 stops working until it is updated. See "Upgrading from v1.0.0" in the README.

- Every path under `/api` now requires a login. Call `POST /api/login` first and keep the cookies it sets.
- Every `POST`, `PUT` and `DELETE` now requires the `X-CSRF-Token` header. The token comes back from the login as `data.csrf_token`.
- `POST /api/service-port` no longer answers 500 when a tunnel fails to start. The row is written and the answer is immediate; the reconcile loop starts the tunnels afterwards. Read the outcome from `GET /api/status`.
- A read that fails because the database cannot be reached now answers 500. It used to answer 404, which was indistinguishable from a row that was never there.
- 500 answers no longer carry the message the database produced. The cause is written to the log instead.
- The `user` table is added. AutoMigrate creates it on the first startup.
- No CORS headers are sent any more. Only a browser page on another origin calling this API is affected; curl and server to server calls are not.

## Add/fix features:

- Built-in UI:
  - The operator UI is compiled into the binary and served at `/ui/`. `/` redirects to it. Nothing has to be deployed next to the binary.
  - Four screens: tunnel status, Hosts, service ports, and login with the first-run setup.
  - The status screen shows how many tunnels should be running next to how many rows exist and how many are connected, and says what the difference between them means.
- Authentication:
  - One account, stored in the database. The first startup creates it and writes an initial password to a file next to the configuration file with permission 0600. The log holds the path, never the password.
  - The first login leads to a setup that takes the username and the password. It deletes the initial password file and drops every other session.
  - The login password is stored as a bcrypt hash. Sessions live in memory for 12 hours past their last use.
- Tunnel Management:
  - Tunnels are now kept up by a reconcile loop that compares what should be running with what is running. Registering or removing a Host or a service port takes effect at once, and the loop re-checks every `reconcile.interval_sec` seconds, 5 by default.
  - A tunnel whose Host address, port, user or password changed is restarted on the new settings. It used to keep running on the old ones.
  - `GET /api/status` reports `desired_tunnels` alongside `total_tunnels` and `connected_tunnels`, so a tunnel that never started is visible.
  - Fixed forwarded connections being cut off when one side closed its writing end. A client that sent its request and half closed received nothing back.
  - Fixed a tunnel monitor leaking on every reconnect, a double unlock in `StopAllTunnels`, and disabled Hosts having their tunnels started.
  - The SSH keepalive carries a deadline, so a peer that stops answering is detected instead of blocking the monitor.
- Security:
  - Host SSH passwords are encrypted at rest with AES-256-GCM. The key lives in the file named by `security.key_file`, created with permission 0600 on the first startup.
  - The startup refuses to run against a key that opens none of the stored passwords, rather than overwriting them.
  - Answers never carry the SSH password of a Host or the password hash of the account.
- Database:
  - A single connect attempt now has a 3 second deadline. Without it one attempt ran until the kernel gave up on the handshake, which took over two minutes and made `database.timeout_sec` meaningless.
  - gorm output goes through the configured logger and level. Statements are traced at `debug` only; a missing row is no longer logged as a failed query.
  - `service_ports.local_port` has a unique index. A database with duplicates has to be cleaned up before upgrading; see the README.
- Platforms:
  - Release binaries are built for Linux amd64 and arm64, macOS Intel and Apple silicon, and Windows amd64. v1.0.0 shipped one binary whose name did not say what it ran on.
  - The startup no longer needs syscall.Rlimit to exist, so the package builds on Windows and on the BSDs. Raising the file descriptor limit is a Unix step and does nothing on Windows, which has no per process handle limit to raise.
  - Only the Linux binaries have been run. The others are checked by the compiler and the vet tool for their platform and no further.
- Operations:
  - The process runs without root. What is limited in that case is documented in the README.
  - The API server shuts down gracefully on SIGTERM instead of dropping requests in flight.
  - A log file that cannot be opened no longer ends the startup; logging falls back to the console.
  - Row locking on the update and delete handlers, so two requests on the same row no longer undo each other.

# v1.0.0

## Add/fix features:
- Tunnel Management:
  - Fixed issue of only listening on loopback address (127.0.0.1)

# v0.1.0

## Add/fix features:
- Tunnel Management:
  - Fixed issue where all tunnels were shown when requesting host status.
  - Fixed issue problem where failed tunnels remained.
- API Updates:
  - Removed unnecessary 's' from multiple API endpoints for consistency.
  - Renamed all instances of "VM" to "Host" for clarity.
- Set default log path under /var/log/ for easier access and management.

# v0.0.3

## Add/fix features:
- Fixed remote connection not initiating from tunnel-manager host
- Added VM enable/disable functionality
- Enhanced tunnel management system:
  - Fixed duplicate tunnel additions and deletions in database
  - Improved race condition handling of tunnels
  - Reset tunnel connection retry count to 0, when connected
  - Optimized tunnel restoration process
- Improved race condition handling in VM and service port operations
- Fixed database data duplication and indexing

**Full Changelog**: https://github.com/jollaman999/tunnel-manager/compare/v0.0.2...v0.0.3

# v0.0.2

## Add/fix features:
- Resolved transaction issues in VM and service port operations to ensure consistency.
- Resolved transaction issues in tunnels, ensuring consistency.
- Added support for saving log files with various options, including path, max_size, max_age, and compression.
- Do not retry when authentication failed
- Fix wrong tunnel stop issue when updating the service port
- Various bug fixes for improved stability.

**Full Changelog**: https://github.com/jollaman999/tunnel-manager/compare/v0.0.1...v0.0.2

# v0.0.1

## Add/fix features:
- Add REST API for managing VMs and service ports
- Add tunnel manager for SSH tunnel management
- Fix transaction issues in VM and service port operations
- Optimize VM and service port CRUD operations
- Add unique constraint on VM IP for active records only

**Full Changelog**: https://github.com/jollaman999/tunnel-manager/compare/v0.0.1...v0.0.1
