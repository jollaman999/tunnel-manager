# v3.2.5

## Bug fixes:

- The log was hard to read on a screen turned sideways, and two things were taking its width. Safari grows the text of a block when the viewport is wider than the block is, which it does to make a page built for a desktop readable on a phone; this page is built for the width it is shown at, so held upright nothing happened and turned on its side the same page had bigger letters than it asked for. It is told not to. The other was the stamp: twenty-eight characters that could not break, taking 220px of a table with three other columns to fit. The date and the time of day are on two lines now, which is half the column for the same information. Measured at 844px, a phone on its side: the message had 361px and the row stood 99px tall, and it now has 409px and the row is 78px, which is what a row is at any width.
- The layout the log takes on a phone held upright is no longer used on wider screens. A previous release extended it to anything narrower than a laptop, which gave a phone turned sideways the layout of one held upright rather than the table a wider screen has room for.

# v3.2.4

## Add/fix features:

- The Settings screen goes to the address the service comes back at, rather than naming it and leaving you to type it. A restart that puts `api.port` or `api_https_enabled` into place moves where the service answers, and the page now counts out the delay the server named for its own exit plus three seconds and opens the new address. It cannot ask first: the new address is another origin behind a certificate the browser has not been given a reason to trust, so a request to it fails whether the service is up or not. The address is a link under the countdown as well, for going sooner or for staying put, and leaving the screen stops the move.

# v3.2.3

## Add/fix features:

- A generated certificate is good for five years rather than 825 days. The old number was chosen on the belief that Apple applies its limit to a certificate the operator trusted by hand; it says the opposite, that the limit is for certificates chaining to a root shipped with the system and that "if you are using a certificate from a user-added or administrator-added Root CA, this change will not affect you". Nothing generated before this release changes on its own: Make a new certificate on the Settings screen is what replaces one.

## Bug fixes:

- The log went back to being a four-column table as soon as a phone was turned on its side, and the caller went back to being a column too narrow for a package path. Measured across such a row: at 568px the message had 124px and the row stood 267px tall, and at 844px, which is a phone on its side, the caller had 105px and wrapped onto three lines. The log keeps its stacked layout until a window is laptop wide, where at 844px the message now has 754px and the caller does not wrap. A laptop window is unchanged.

# v3.2.2

## Bug fixes:

- Turning HTTPS on and pressing Restart left the screen waiting ninety seconds and then saying the service never came back. It had come back within seconds and on the same port: the page was loaded over http, the service answers that with a redirect to https once it is on, and the certificate behind that redirect is one nobody signed for, so the browser refuses it and the page sees nothing. The port a restart moves to was already followed and the scheme was not, which is the likelier of the two to move, because turning HTTPS on is something you do from the screen being served without it. The screen now names the address the service will answer at and stops waiting on the one it knows is being left.
- The log was unreadable on a phone. Four columns across that width left the message a column a few words wide, running down the screen as a tall thin ribbon: measured in a 390px viewport, 58px wide and 462px tall. Below the width the rest of the screens already reshape at, a log entry is laid out as a block instead, with the time, the level and the caller on a dimmed line and the message across the whole width under it. The same measurement gives 362px wide and 63px tall. At full width the table is what it was.

# v3.2.1

## Bug fixes:

- The dial to the forwarded service had no bound on it, and waited for as long as the kernel retries a SYN. A backend that a firewall drops silently, rather than refuses, held a goroutine and a socket for every connection made through the tunnel for over two minutes, long after the client that opened it had given up. It is the ten seconds the SSH dial already uses. Measured against an address that swallows the handshake: the connection was given back after 135.68s before, and after 10.01s now.

## Notes:

- A test that checks a reset service connection is reported was racing the dial it depends on, and failed on a loaded machine: 17 runs in 20 under load, none on an idle one. It puts a byte through before breaking anything now, which fails 0 in 20 under the same load. Nothing in the program changed for it.

# v3.2.0

## Add/fix features:

- Restart from the Settings screen:
  - The process runs itself again in place of itself, so it keeps the process it already is and the supervisor never sees it go. An installation that was started by hand comes back too.
  - Windows has no such call. There the process ends and whatever supervises it takes over, and the screen says so before the button is pressed rather than after.
  - The tunnels come down in order and the log is flushed by hand first, because a tunnel left up holds a listener open on the far side of the SSH connection and the call that replaces the image does not run deferred functions.
  - When `api.port` is one of the settings waiting for the restart, the screen names the port the service will come back on instead of promising the one it is on. It also stops waiting, rather than asking an address it knows is being left and reporting a service that never came back.
- The username and the password are changed from the Settings screen:
  - Both take the current password, the same reason the setup that opens the account refuses to run twice: a session left open would otherwise be enough to take the account over.
  - Every other session ends when either changes, the username included. A session is bound to the account rather than to the name, so renaming alone would leave whoever already holds one exactly where they were.
  - The new password is typed twice, here and on the setup screen. The second box never leaves the browser.
- A Host logs in with an SSH key:
  - A Host carries a private key, with a passphrase when the key has one. Both are sealed in the row the way the password is, so what lands in the database is never the key itself, and neither is ever in an answer the API gives.
  - The key is offered before the password, which is the order SSH tries them in. A Host that carries both stays reachable with its password while a key that was just registered is not yet the one the far end knows.
  - What is pasted is read when it is stored rather than when something connects. A key that is not PEM, one that wants a passphrase it was not given, and a passphrase that does not open its key are each turned away by name.
  - The key box takes a file dropped on it. It is read in the browser, so only the text is sent.
  - A Host may now carry a key alone. `password` is no longer required.
- What is stored but not being run on:
  - The Settings screen shows which settings are stored with a value this service is not running on, and what each is running on meanwhile. It comes from the server rather than from the last save, so it is there whenever the screen is opened and to whoever opens it, and a restart clears it because the two become the same.
  - A setting that takes hold at once is left out of that list by the same table the save reads.
- `-reset-settings` says what it leaves alone. The registered hosts, the service ports, the account and the certificate are in the same file and are untouched. It is the command for a server that will not start, and half of what it did was the half nobody needed to know.
- The Save under Serve over HTTPS is coloured like the other buttons that are the main thing on their card, and no longer sits against the certificate table below it as though it stored that too. The product name has a line of its own above the tabs.

## Bug fixes:

- A Host whose private key was replaced kept its tunnel on the key it was built with. The fingerprint a reconcile pass compares against was taken over the addresses, the user and the password alone, so the save answered as though it had taken and the new key went untried until something else dropped the connection. Measured against a server that accepts only the key: a key the server refuses left the tunnel connected for a minute, and now the tunnel reports the refusal within five seconds.
- A Host registered with a key alone had its empty password sealed into the row on every reconcile pass, so it came to carry a password that is the empty string and offered it on every connection.

## Notes:

- The tunnel was measured under load: 33 million requests and 416GB through six tunnels at once, with no response reaching the wrong client, no data race, and no goroutine or descriptor left behind. A tunnel carries 30 to 60 percent of what the same backend serves directly. What costs that is the single write lock an SSH connection multiplexes through, which is in the SSH library rather than here, and three quarters of what this program itself spends is the read and write calls a userspace proxy is made of. Enlarging the copy buffer was measured and changed nothing.

# v3.1.0

## Breaking changes:

**The address to open is `https://`, not `http://`.** The API and the web UI are served over TLS from this release on, and a certificate is made on the first start if the database holds none.

- A request that arrives in the clear is answered with a `307` redirect to the same address under `https`. A browser follows it. `curl` does not follow a redirect unless it is told to, so a script that calls `http://` gets the redirect itself and nothing else.
- Nobody signs for the certificate that is generated, so a browser warns about it and `curl` refuses it with exit code `60` until the certificate is trusted or `--cacert` points at it. The Settings screen shows the certificate as PEM for that.
- `api_https_enabled` turns this off and puts everything back in the clear. It is on by default, including for an installation that is upgraded, where the column is filled in rather than left empty.

## Add/fix features:

- HTTPS:
  - The certificate and its private key live in the database beside the settings, so one file is still the whole of an installation. The private key is sealed with the same encryption key the SSH passwords use, and a database file that is taken does not carry the key in it.
  - The generated certificate is an ECDSA P-256 certificate valid for 825 days, which is the longest a browser accepts. It covers `localhost`, the loopback addresses, the host name of the machine and the addresses of its interfaces.
  - It is still one port. A connection is sorted by its first byte, which is `22` for a TLS handshake and a letter for an HTTP method, so nothing new has to be opened in a firewall.
  - The sorting is done per connection and never in the accept loop. A client that connects and sends nothing holds up no one else, and is closed after ten seconds.
  - A stored certificate that cannot be used, because it expired or because the encryption key no longer opens the private key, is replaced rather than reported as a failure that stops the start. Why it was replaced is logged, since a fingerprint that changes on its own is what a client warns about.
- The certificate on the Settings screen:
  - The fingerprint, the subject, the issuer, the names it covers and how long it has left, with the certificate itself as PEM to take into a trust store.
  - **Make a new certificate** generates and installs one. **Register certificate** takes a certificate of your own as PEM, the intermediates behind it included.
  - Neither restarts the process. The certificate is handed out per handshake, so what is installed is served from the next connection on, and the connection the button was pressed on is not dropped.
  - What is pasted is read before it is stored. A key belonging to another certificate, a box filled with the other box's content, a key still protected by a passphrase, a certificate that ran out, and one whose extended key usage does not include `serverAuth` are each turned away by name rather than accepted and served.
  - A certificate that is not valid yet is stored with a warning rather than refused. Two machines disagreeing about the time by a minute is ordinary, and refusing it would leave nothing to register.
  - A replacement is logged with the fingerprint before it and the one after.

## Notes:

- The private key is never sent to the screen and is not in any answer the API gives.
- The redirect is a `307` rather than a `301` or a `302`, which turn a `POST` into a `GET`, and rather than a `308`, which a browser caches: HTTPS can be turned off, and a cached permanent redirect would outlive that.
- HTTP/2 is not offered. Nothing these screens do needs the multiplexing, and leaving it out keeps one protocol on the wire.
- Both READMEs now carry a section on HTTPS: how to reach the server, what the browser warning is and what to check it against, how to register a certificate of your own, how to renew, and how to turn HTTPS off.

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
