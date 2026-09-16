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
