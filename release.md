# v3.13.2

## Add/fix features:

- **The Manual no longer says the Status screen leaves local forwards out.** It said local forwards were not counted there, which stopped being so in v3.13.0 when they were put in the same table as the tunnels, and again in v3.13.1 when the counts were made to cover both. It now says where they stand and what the Port reached column of a forward is a reading of.
- **The Manual names the columns the Status screen actually has.** It walked through the three addresses of a tunnel as Server, Local and Remote, and the last two were renamed Opened and Reaches in v3.13.0 because the table holds forwards that run the other way. It gives the names in use and says why they are not Local and Remote.
- **The Manual says a local forward is measured too.** The passage on Port reached described the connection made to the Host and nothing else, so a reading on a forward row had nothing behind it. It says that a forward is measured at the same moments and the other way about, the target being dialled from the Host.

All three are wording on the Manual screen, in every language. Nothing else changed.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.

# v3.13.1

## Add/fix features:

- **The counts over the status table are four, and each counts the tunnels and the local forwards together.** They were six, three about the tunnels alone and three about the local forwards, over a table that had just been made to hold both sorts in one. A local forward is a tunnel to whoever is reading the screen, so the counts read Desired, Connected, Reconnecting and Errors over everything in the table.
  - The count of rows is gone. The table says how many rows there are, and the page controls under it say so in words, so a box over the table holding the same number was the screen counting its own rows back at the reader.
  - Errors and Reconnecting are apart because they leave the reader with different work. A row that is reconnecting is on its way back by itself and one in error is waiting for somebody.
- **A local forward says whether its target answered.** The Port reached column was empty on a local forward, because nothing had ever measured one: the target was only ever dialled when a client turned up on the local port. It is dialled once from the Host over the same SSH connection a client would use, as soon as the forward comes up, and the column reads reachable or unreachable the way it does on a tunnel.
  - The advice about GatewayPorts and PermitListen that sits under a tunnel whose port did not answer is not drawn under a local forward. Every line of it is about a port the SSH server was asked to open on the Host, and a local forward asks for no such port.

## Notes:

- `GET /api/status` no longer answers `total_tunnels`, `total_local_forwards`, `connected_local_forwards` or `desired_local_forwards`. A script that read any of them reads nothing now.
- `desired_tunnels` and `connected_tunnels` keep their names and count more than they did: the local forwards are in them as well as the tunnels. `reconnecting_tunnels` and `error_tunnels` are new and count both sorts too. `total_rows` is unchanged and is what the pages are cut from.
- `forward_reach` is on a local forward row as well now. On a tunnel it says whether the port opened on the Host answered a connection from here; on a local forward, whether the target answered one dialled from the Host.
- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.

# v3.13.0

## Add/fix features:

- **The status screen holds the local forwards beside the tunnels.** It read the tunnel table alone, so a local forward was somewhere else entirely although it is the same thing seen from the other end: an SSH connection to a Host carrying a port. Both are in one table now, one page at a time, with a Kind column saying which sort a row is.
  - The columns that named a side are gone. A service port is opened on the Host and read from here, a local forward is opened here and read from the Host, so Local and Remote meant opposite things on the two sorts. They are Opened and Reaches, and each cell says which machine it is talking about, since the addresses themselves look alike.
  - The counts above the table are six rather than three. Folding them together would make one number out of two things that are fixed in different places.
  - `GET /api/status` answers both sorts in `tunnels`, each row carrying `kind`. A row of a local forward carries no `sp_id` and no `forward_reach`. The three counts that were there keep counting the tunnels alone, and `total_local_forwards`, `connected_local_forwards`, `desired_local_forwards` and `total_rows` are beside them.
- **A local forward is numbered within its Host.** It was known by a number that ran across the whole table, so the first forward of the second Host was 4 and nothing said why. The Host and the place on it are what a forward is named by, and the first forward of one Host and the first of another are both number 1.
  - The three paths that read, change and delete one are `/api/host/:id/local-forward/:number`, where they were `/api/local-forward/:id`. Adding and listing are unchanged.
  - A forward added is given the lowest number its Host is not using, so the gap a delete leaves is filled and the numbers of a Host run 1, 2, 3 with nothing missing.
- **The icons are drawn from the full size artwork**, and the icon sits beside the title of every README.
- **The animation at the top of the README was recorded again**, with a scene for the status screen holding a row of each sort.

## Notes:

- **The first start after this upgrade numbers the local forwards that are stored.** Each Host's forwards are numbered from 1 in the order they were added, which is the order the panel has been listing them in, and the move is one transaction: an installation that fails half way through comes back up on the table it had. A file exported from an earlier release carries no numbers and is unaffected.
- A script that reads `GET /api/status` and counts the rows of `tunnels` as tunnels now counts the local forwards with them. `kind` is what tells them apart.
- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.

# v3.12.1

## Add/fix features:

- **A local forward can be switched off.** One that is switched off opens nothing and makes no SSH connection, while it keeps its local port, so no other forward, proxy or setting can take the port while it is off. Its state reads off, apart from disabled, which is still what a forward on a disabled Host reads.
- **The local forwards of a Host are listed the way the other lists are.** The panel has checkboxes, pages, buttons for the checked rows (Enable, Disable and Delete, folded into one menu where they do not fit), and Edit, Enable or Disable and Delete on each row. Add sits at the top right of the list, and Add and Edit open the form in a second window over the panel: a save closes it and says so on the window, and a refusal is shown as before while the form stays open with what was typed.
- **The list of a Host's local forwards takes page and size.** `GET /api/host/:id/local-forward` answers the shape the other lists do, the rows under items with total, page and size, where it answered a bare array before. A script that read the array reads items now.
- **The Korean screens call the machine tunnel-manager runs on the system.** The local forward and SOCKS5 wording added in v3.12.0 used a different word for it than every other Korean screen does.

## Notes:

- The database gains enabled on the local forwards. The first start switches on every local forward stored before, so nothing that was running stops.
- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.

# v3.12.0

## Add/fix features:

- **A Host can carry local forwards.** A local forward runs the other way from a tunnel: this machine opens a local port, and every connection to it goes over the SSH connection of the Host to a target the Host reaches, which is what `ssh -L` does, kept up the way a tunnel is. They are added, changed and deleted from the Local forwards button in the row of a Host, and through `/api/host/:id/local-forward` and `/api/local-forward/:id`.
  - A local port opens on every interface unless the forward is set to this machine alone, and it cannot be the port of another forward, of a SOCKS5 proxy or of this server.
  - A forward runs while its Host is enabled, is built again when the connection drops, and stops on a refused login or host key until that is put right. Its state is shown in the panel.
  - The tunnel file carries the local forwards of each Host. A file from an earlier release leaves the local forwards of a Host as they are.
- **A Host can open a SOCKS5 proxy.** A box on the Host form turns it on, with the port (1080 to begin with), where it is opened and which client addresses it answers. A browser set to the proxy reaches whatever the Host reaches, names included, since they are looked up on the Host. It answers CONNECT and asks for no password, so an empty list of client addresses on every interface lets anyone who can reach this machine into the network behind the Host. The Host list shows the port and the state of the proxy.
- **The server starts on another port when the stored one is taken.** It used to stop, which left no screen to change the port on. The port it moves to is logged with the reason and is not stored; the next start tries the stored port again, a restart from the Settings screen tries the one it was running on first (on Linux and macOS; on Windows the service manager starts it afresh), and the Settings screen lists the port as waiting for a restart.
  - A port the stored API port meets on a local forward or a SOCKS5 proxy is refused on the Settings screen and on a settings import, with a window that offers to move either one. `-reset-settings` warns when it puts the port back onto one of them.
- **Windows is looked after.**
  - A Windows installation starts again on the data it made. The check on the mode of the key file refused every start after the first.
  - The key file, the initial password, the database with its journal files, the logs and the directories made for them are kept to their owner, SYSTEM and Administrators, and files an earlier release left readable to other accounts are narrowed at startup with a line in the log.
  - A port held by another program on any address, or kept by Windows for itself, is seen as taken by the server, the local forwards and the proxies.
- **The SSH handshake is bounded by the connection timeout.** A server that accepted the connection and said nothing held a tunnel from ever trying again.
- **The screens:**
  - The card is wider on a wide window, so the Host list shows its buttons on one line; running text keeps its old measure. A list too wide for its window scrolls sideways, and the buttons over it that act on the checked rows fold into one menu where they do not fit.
  - The status badges, and the kind, the action and the reason of every row an import reports, are said in the language of the screen, and a settings import shows what it put back to the default.
  - After a row button, a button over the checked rows or a closing panel draws the list again, the keyboard focus goes back to where it was rather than to the top of the page.
  - The pages carry an icon, also answered at `/favicon.ico`.
  - The Manual screen explains local forwards and the SOCKS5 proxy.

## Notes:

- The database gains a table for the local forwards and four columns on the Hosts for the proxy. They are added on the first start; a proxy is off on every Host that was stored before.
- On Windows the first start narrows who can read the files named above, and says so in the log.
- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.

# v3.11.2

## Add/fix features:

- **A save of the settings says what it did rather than that it was made.** The Settings screen read the answer and told the three cases apart; the HTTPS switch and the Update screen said the setting was stored whatever came back. So turning the switch to the value it already had said it was stored and would be taken up at the next start, and changing how often a release is checked for said only Saved although it waits for a restart. The three read the same answer now.
- **Signing out, and setting the account up, are said on the window.** Both were carried to the screen they land on as a line above it, which on the login is a line in the way of the box to type in and on the status screen is a line over what the reader had just got to. The line that asks for a username and a password stays where it is: it is what to do rather than what was done.
- **A setup that somebody else had already made is said as the refusal it is.** It was written as an information line, which is what a screen says when nothing went wrong. It goes up on the window and stays above the login, as every other refusal does.
- **The Update screen says check where it used to say look.** Its own keys, its own server fields and every other language already said check: the button read Look now, and update_check_interval_hours was labelled Look every (hours).

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about the database changes in this release.

# v3.11.1

## Add/fix features:

- **A box that is checked is called checked.** The English screens said a row was ticked while every other language already says it in its own word, and the box itself is checked in the markup and in what a screen reader announces. The one tick left is the period the reconcile loop runs on, which is not a box.
- **A save that changed nothing says so rather than saying it was stored.** Being stored is the part nobody is asking about at that moment.

## Notes:

- Only the wording changes in this release. Nothing behaves differently.
- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about the database changes in this release.

# v3.11.0

## Add/fix features:

- **A message that says something went through is shown on the window rather than above the screen.** A form is often submitted from well down a long list, and a line written above the screen is then somewhere the operator is not looking. It appears at the top of the window wherever the page is scrolled to and takes itself away after a few seconds.
  - **A refusal is shown in both places.** It goes up on the window so that it is seen, and stays above the screen so that it can be read: a refusal is read rather than glanced at, and is often longer than a few seconds of reading.
- **Changing the language says the message again in the new words.** A message on the screen was the finished sentence, so it stayed in the language it was written in. It is now held as the making of the sentence and made again whenever the page is drawn. A refusal from a server this screen has no wording for stays as the server sent it, there being nothing to make it from.
- **A failed install is written above the screen as well as on the window.** The line waited for whatever drew the screen next, which on a screen that refreshes nothing is the next thing that is pressed.
- **The service ports of a Host are drawn as a table.** The panel listed them as rows of its own making while the two screens behind it draw the same things as tables, so the same service port was read one way on one and another way on the other. It is the same table now, with the same headings and the same box on the heading that takes the page.
  - The box says nothing when it is pressed. On the lists a tick is a selection and what it selected is worth a count; here a tick is the assignment itself, and the panel already says that nothing is stored until it is saved.
  - What is ticked still outlives the page it was ticked on, which is what this panel differs in: a tick dropped on the way to the next page would be an assignment taken away from a Host without anybody saying so.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about the database changes in this release.

# v3.10.1

## Add/fix features:

- **The Update screen says how the install turned out.** The page reloaded itself when the install was over and said nothing about it, so whether the release went on was something to work out from the number at the foot of the screen. What was running and what was asked for are written down as the install starts, and are read once the page comes back with both a version and the words to say it in.
  - A version other than the one asked for is named rather than called a failure. The install fetches the newest release, and a newer one may have landed between the check and the press.
  - The message is said once and then forgotten, and one left behind by a page that was closed mid-install is dropped rather than shown late.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about the database changes in this release.

# v3.10.0

## Add/fix features:

- **The rows of a list are ticked and acted on together.** The Host list and the service port list carry a box on every row and one on the heading that takes the page. A tick reaches the rows of the page it was made on and no further, so a press never touches a row nobody looked at.
  - **Ticked rows are deleted in one press.** What is about to go is put in front of you as a list first, because the rows do not come back and a count is not something to agree to. Each row is still its own request, so one that is refused leaves the rest deleted and comes back named, with what it was refused for.
  - **Ticked Hosts are turned on and off in one press.** A Host already in the state being asked for is passed over rather than written, and what was passed over is counted in what comes back.
  - **Ticked service ports are assigned to Hosts from the service port list.** An assignment could only be made from the Host that was to carry it, so putting one service port on twenty Hosts meant opening twenty panels. The reach is written on the assignments the press creates, and a Host already carrying one keeps the reach it has.
- **A page of service ports is assigned to a Host in one press.** The panel that assigns them takes the page it is showing and leaves the other pages as they were, and it says how many rows it took, since clearing the box takes an assignment away from each of them.
- **The row of controls over a list carries page numbers.** The way through a list was the page on either side and a line saying where in it you were, so a list of twenty pages was reached by pressing Next nineteen times. The numbers around the page being read are there now, with a pair of buttons that move that run of numbers without moving the page, so a page far from this one is one press away. The run comes back to the page being read as soon as the page turns.

## Notes:

- The row of controls still appears only once a list is longer than the smallest page size, which is ten.
- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about the database changes in this release.

# v3.9.2

## Add/fix features:

- **A container image is published for each release.** Tagging a release now builds `linux/amd64` and `linux/arm64` and pushes them to `ghcr.io/jollaman999/tunnel-manager`, under the version and under `latest`. `docker-compose.yaml` runs that image instead of building the working tree, so a compose up brings up the release rather than whatever happened to be checked out. Write a version in place of `latest` to stay on one.
- **The language picker and the theme switch sit on the line of the title on the login screen.** They were eight pixels below it. Those screens carry the two on the row of the heading itself, and a heading carries the space that goes under it; centring in that row centres each item with its space, so the title sat above the middle of a row its own margin had made taller and the switches sat on that middle. Measured at four widths, the two are on one line now, and neither the title nor anything under it has moved.
- **The screens call a Host a Host wherever the menu does.** The same thing was named two ways from one sentence to the next. An SSH host key is not the entity, so it stays as it was and only the entity follows the menu. The same was done for Service Port, in English, Spanish and Portuguese.
- **The notice that follows an assignment reads correctly for a count of one.** It carries two or three counts but only one of them could pick which wording was used, so it read "1 service ports assigned" whenever the reach was what had changed. The counts sit behind labels now, which no language has to agree with.
- **The panel that assigns service ports says that ticking is not saving.** It told the reader a tick survives the other pages and left them to work out that surviving is not being stored.
- **The reference documents the three endpoints that approve a host key**, in all four languages it is written in, along with the two host key counts, `error_kind` and `listen_addresses` that `GET /api/status` returns.

## Notes:

- Saving settings binds the request to a body of its own that names only the sixteen fields the screen sends. Nothing was writable that should not have been; the boundary is now written down so that adding a field the server owns cannot open one.
- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about the database changes in this release.

# v3.9.1

## Add/fix features:

- **The Installing screen waits for the service and loads the page itself.** It said the install had started and left the operator to load the page again by hand, because nothing on this end is told when an install finishes: the process that would say so is the one being replaced.
  - What it watches for is a different version answering rather than the server answering at all. The service is stopped only after the release has been fetched and checked, so for the whole of the download it is the old process that answers, and a page that took an answer for the install being over would reload onto the version it started from and call it done.
  - Three minutes is the longest it waits. The end that fetches allows the download two minutes on its own and the restart follows that, so a page giving up at two would reload in the one window where nothing is there to answer. It costs nothing when things go well, because the wait ends as soon as a new version answers.
  - The bar is how much of that wait has gone by and not how far the install has got, which is not something this end knows. Its width is written on each ask rather than moved by a transition, so it never glides on through a gap where nothing was learned.
- **The addresses of a forward are drawn only where they differ from what was asked for.** On a forward that went up the way it was asked to, all four sentences said so, under every row of a screen of tunnels. It is drawn now where one of the two requests was turned down, where the port did not answer a connection this end could open, or where the Host, asked what it has open on that port, named something other than what was asked for. The last of those is the one worth the room: a server set to bind every interface ignores a request for the loopback and opens the port to its whole network.
- **A count is said the way each language says it.** A catalog holds a sentence for one and a sentence for many, and two are not enough everywhere.
  - Russian puts 21, 31 and 101 in the same class as 1, so the screen said one other client had been signed out when it had been twenty-one, and called twenty-one hosts that one host.
  - Arabic tells apart none, two, a few and many, and had the form for a few standing in for all of them.
  - Chinese, Vietnamese and Thai have no plural at all, so their sentence for one is never chosen and their sentence for many was pointing at one thing with a word for several.
- **A path in an example is no longer drawn in pieces on a screen that reads right to left.** The hint under a stored path built its example from the directory the database is in and a tail written into the catalog. The directory is a value and is laid out as one run; the tail is not, so the end of the path came before its beginning. The whole example is one value now.
- **An address is no longer turned round on a screen that reads right to left.** Each value written into a sentence was held apart on its own, which is right for a value standing alone and wrong for two the sentence joins: the colon between them took the direction of the sentence and the address came out as the port, the colon, and then the host. Measured in a browser before and after, on the same string.
- **Every catalog was read as the language it is for.** They passed every check there is and still read as translations.
  - Three had a sentence the wrong way round. "Host is trusted on this key" is this end deciding to trust that Host, and Russian, Portuguese and Vietnamese had the Host doing the trusting, which over SSH is a sentence about the far side's authorized_keys and a different thing entirely.
  - Vietnamese called emptying the log file deleting it, in a sentence that then said what the file holds from now on. Arabic had the two words of one label in the other order from the forty-nine places the same catalog names it, which in a right to left script is a different order of code points. Arabic and Hindi both labelled the box that takes a number of hours as asking how many times to look.
  - The rest is a word doing two jobs: a German column header that reads as "deleted" over a column of addresses, a French catalog naming the registered thing two ways, a Japanese one calling the same file exported and written out in the same table, a Korean one calling the same start two words in the same sentence, a Hindi one where updates were written in a different register from the other eight hundred keys.
- **The Update screen says in every language what the English says.** The English and the translations of that one screen went in together saying different things, so it was never a sync that lapsed. What the eleven were missing was mostly the part that warns: the advice beside the automatic install had neither that nobody is asked nor that it can happen at any hour, and the sentence about a version that cannot be compared left out that nothing installs itself while that is so.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about the database changes in this release. An installation on v3.9.0 upgrades with nothing to migrate.
- The reading of what a BSD or a Mac answers when asked what is listening is still written from the manual pages of those systems and has not been run against one.
- None of the twelve translations has been read by a native speaker. What each change rests on is a count of that catalog's own usage and a sibling string that already said it the other way.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.

# v3.9.0

## Add/fix features:

- **How far a forwarded port reaches is chosen on the assignment, and both addresses of the choice are opened.** v3.8.3 put a bind address on the Host, and that was the wrong place for it: one Host carries several service ports, and one of those may be meant for that machine alone while the next is to be reached from elsewhere. Holding one answer for the whole Host forces those two together. The pair of a Host and a service port is the smallest row that can hold both, and it is the row a forward already is.
  - The choice is a reach rather than an address: the Host itself, or every interface. Each names a pair, `127.0.0.1` with `::1` or `0.0.0.0` with `::`, and both are asked for. The two families do not stand in for each other, and a port opened on one is not reached by a client that connects to the other.
  - The address that was typed by hand is gone. This screen never asks a Host what interfaces it has, so an address written here was a guess, and a wrong guess fails as a forward that never opens and says nothing about why.
  - Three places ask: the Host form when it is to carry every service port, the service port form when it is to go to every Host, and the panel on a Host row, which applies a reach to what is ticked or edits one row on its own.
  - **A bulk apply moves only the rows that are ticked.** Adding one service port used to be a request that carried every port already there, and reading it as a reach for all of them would widen a Host somebody had pinned. The rows to move are a list of their own for that reason.
  - An assignment that names no reach is opened on every interface, which is what every forward was opened on before there was a column to say otherwise. Nothing that is running changes reach on the upgrade.
- **The status screen says what was asked for, what was confirmed, and that the rest is unknown.** It used to say the address was refused and that nothing dialling it arrives, and neither is something this end knows.
  - **What an SSH server answers to a forward request is not a measurement of what it bound.** Measured against OpenSSH: a server carrying `GatewayPorts yes` takes both families on the first request and refuses the second, so a request it refused can be a port that is up. It never says why it refused, and a server with no IPv6 at all gives the same answer.
  - `GatewayPorts` also decides the binding regardless of what was asked. Its manual page has `yes` forcing the wildcard and `no` forcing the loopback, so `clientspecified` is the one setting under which the reach that was picked is the reach that is bound. The form says so.
  - **An assignment on the Host itself can never be confirmed from here, and that is not a fault.** Its ports are on the Host, which nothing outside that machine reaches. That silence is written down as unknown rather than as a port that cannot be reached, so a tunnel doing exactly what was asked of it is no longer drawn as one that failed.
- **The Host is asked what is actually listening on the forwarded port.** It is the only reading that survives a server ignoring the reach that was picked, and there was no way to get it: the reply to a forward request carries a port and no address, and dialling the port reaches one address family of the one address this program holds.
  - It is asked over the connection that is already there, once the forwards are open, and it is asked best effort. An account with no shell refuses the session, which leaves the forwards standing and the answer empty. There is no setting to turn this off, because an account without a shell already is one.
  - **Empty means the question went unanswered, never that nothing is listening.**
  - `ss` is asked first, about the one port, and `netstat` after it. What comes back is read for the shapes of Linux, of the BSDs and of macOS, and a Host whose output is none of them leaves the answer empty rather than a guess.
- **The API is described in OpenAPI and the description is served with a Swagger UI.** Every call is on a page at `/ui/api-docs/`, with the fields it takes and the answers it gives, and the button on each of them sends a real request to the server being read. The description is at `/ui/openapi.json` for a client generator or for Postman.
  - Both come out of the binary, so they open on a machine that reaches this server and nowhere else. Nothing is fetched from the network to draw the page.
  - A test fails where a route is registered that the description does not carry, and where it carries one no route answers, so the two do not drift apart quietly.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- **An installation that set a bind address on a Host in v3.8.3 has that answer carried to every assignment of that Host, once.** Nothing that was held to the loopback is widened by the upgrade.
- `hosts.bind_address` joins `service_ports.bind_address` as a column that is not read any more. Nothing removes either, which is harmless.
- An exported configuration carries the reach of each assignment. A file written by v3.8.3 carries the bind address of a Host instead, and that is read and turned into the reach of every assignment of that Host by the same rule the upgrade uses. It is never written back out.
- The reading of what a BSD or a Mac answers is written from the manual pages of those systems and has not been run against one. A shape that is read wrongly yields nothing rather than an address that is wrong, which is what not being able to ask already means everywhere here.
- What was looked at and deliberately not changed is written down in `docs/design/2026-09-23-security-and-bind-address.md`, the measurements against OpenSSH among it, so the same ground is not covered again from nothing.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.8.3

## Add/fix features:

- **A Host says which of its addresses the forwarded ports are opened on.** Every forward was asked for on the wildcard and there was no way to ask for anything else, so on a Host whose SSH server carries `GatewayPorts yes` the port was open on every interface that machine has, reachable by whatever is on its network rather than only by this manager. The screens measured that reach and offered no way to narrow it.
  - The address is the Host's and not the service port's. What opens the port is the SSH server over there, so which of its interfaces it opens on is a fact about that machine; a service port carried to three Hosts could want three answers.
  - The choices are the wildcard, the two loopbacks and whatever is typed. They are not read off the Host: this program opens no session on a Host and runs nothing there.
  - A Host that names no address is asked for on the wildcard, which is what every Host was asked for before. Nothing that is running changes reach.
  - The form says what the wildcard means for as long as it is the one chosen, and the Host list carries the column, so which Host is open on every interface is read off the list rather than found by opening each row.
- **A wrong account password is counted wherever it is asked for.** Six calls ask for it again before they do something that cannot be taken back, and the limit was on the login alone. The scenario those calls name as their reason, a session left open on an unattended screen, was the one it did not cover: emptying the log in a loop guessed at the password as fast as the network carried requests, and spent a bcrypt compare of a root process per try. They count on the counters of the login, because two sets of counters are two allowances to spend.
- **A redirect is sent only to a name the certificate carries.** The plaintext port built its Location from the Host header, so anything could ask it to point somewhere else. The names are read from the certificate being served and read per request, since renewing or installing one swaps it while the process runs.
- **The download of a release is held to the same hosts a redirect is held to.** The allow-list ran on the second hop and later. The addresses the files are fetched from are fields of the document the API answered with, and a document is no more this process's to trust than a redirect is.
- **One password-sealed file is opened at a time, and a file may ask for less memory.** A sealed file states the parameters it was written with, so the file being opened decided how much was asked for, and nothing decided how many arrived at once.
- **The two cookies are guarded where the connection lets them be guarded.** They went out under names any sibling on the same host name could write, and a cookie ignores the port. Nothing was taken that way, because the CSRF header is compared against the token the session holds, but the two could be overwritten and the client left unable to use the page.
- **Sessions that ran out unseen are dropped.** A client that closes its window instead of logging out left a token that was never sent again, and the only eviction was in the lookup of the token being looked up, so its two deadlines passed with nothing reading them.
- **A save says so where the reader is looking.** The line that said it was drawn at the top of the screen, and the forms that raise it are at the foot of long ones. It is now a box of its own that does not scroll away and takes itself off after three seconds. A refusal stays the line it was: it has to be read and acted on.
- **A value written into a sentence keeps its own direction.** On a page that reads right to left the slash of a path was carried to the far end of the sentence, so `/var/log/tunnel-manager.log` was read on the screen as `var/log/tunnel-manager.log/`. What a machine wrote is now laid out the way a machine wrote it, which a line of JSON in the log needed as well.
- **The theme switch carries a sun by day and a moon by night**, and the note under a row of presses is no longer against them.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- **Everyone signed in is signed out by this upgrade.** The cookies are named differently now, so the ones a browser is holding are not read. Nothing is lost by it; sign in again.
- An exported configuration carries the bind address of a Host. A file exported from an installation where a Host is held to the loopback puts it there wherever the file is taken in.
- A database that ran a build made between v3.8.2 and this release carries a `bind_address` column on the service ports as well. It is not read any more and nothing removes it, which is harmless.
- What was looked at and deliberately not changed is written down in `docs/design/2026-09-23-security-and-bind-address.md`, findings and scanner results together, so the same ground is not covered again from nothing.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.8.2

## Bug fixes:

- **Installing an update from the Update screen did nothing but take the service down.** The press was answered, the release was downloaded and its checksum checked, and then the service stopped and stayed stopped with the old executable still in place.
  - An install stops the service before it replaces the executable, because a file that is open for execution cannot be written to. systemd stops a unit by signalling everything in its cgroup, and the install was started as a plain child of the service, so it was killed by the stop it had just asked for, three lines before the copy it was there to do.
  - Nothing brought the service back either. An explicit stop is not a failure to a service manager, so `Restart=always` does not act on one.
  - The same command run by hand from a shell worked, and still does. A shell is not in the cgroup of the service, which is why this was only ever broken from the screen.
  - On a machine with systemd the install is now handed to systemd as a unit of its own. The stop does not reach it, and what it says goes to the journal with everything else this service writes. Where there is no `systemd-run` it stays a plain child and writes its report to a file beside the log rather than to nothing.
- **An install that fails after stopping the service now starts it again.** Every way out between the stop and the start left the service down with nothing about to start it. The failure that is reported is still the one that happened.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- An installation running v3.8.0 or v3.8.1 cannot install this release from the Update screen, because the code that would do it is the code this release fixes. Run the install once by hand and the screen works from then on.
- A release left a directory under the temporary directory on every attempt that was killed this way, holding the downloaded executable. Nothing removes those on the next attempt; they can be deleted.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.8.1

## Add/fix features:

- **The screens are drawn as panes of glass.** Every surface was a flat fill, and the one colour the UI carried was a navy that had been chosen before any of the screens above the Status one existed.
  - A card and the page it is on are tinted, blurred over whatever is behind them, and lit along the top rim. A browser that does not know how to take that blur is left with the tint, which is opaque enough to stand on its own: the page is flat there rather than broken.
  - The accent is a violet, and the press a card is there for is filled with a gradient of it and throws a little of that colour onto what is under it. A press that is not the main one is a tint of the same violet rather than the colour of the pane it sits on, which is how it came to be invisible.
  - Green and amber are mixed again as tints of the same kind, so what is said in them reads as belonging to the page rather than as having been pasted onto it. Every pair of text and fill was measured on the drawn screen and not on the values: the least of them is 4.5 to 1.
  - A ticked box is the colour of the press beside it, in a deeper mix than the accent: the accent is made to be read as text on a pane, and a box a few millimetres across filled with it is a smudge of colour rather than a tick.
- **A first visit starts in the dark.** What the browser prefers is a guess made about every page at once, and this one is a console left open beside other work. What is picked here is still kept and is still what the next visit starts from.
- **The theme switch is a knob on a track.** It carried a sun that set into a crescent moon, clouds that drifted and stars that came out behind it. At the size it is drawn none of it read as what it was meant to be.
- **The language list and the theme switch sit on the row the name is on.** They were fixed to the corner of the window, so they stayed over the page while it scrolled and sat on top of whatever was under them.
- **A refresh of the Status screen no longer takes back what was dragged into view.** A wide table is in a scroller of its own and the message under a failing tunnel is read by dragging it sideways; every refresh built a new scroller, which starts at its beginning, so five seconds later the end of the message had slid back off the screen.
- **A refresh waits while the page is being handled.** A scroll inside a box does not reach a listener waiting at the window, a mouse held down was not watched at all, and text that has been selected was dropped by the next draw with the sentence still on the screen and the highlight gone.
- **The note under the update presses is no longer against them.** It read as part of the press rather than as a note about it.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about what this program does to a Host has changed in this release. It is the screens, and what they do while they are being read.
- The reference is written up to the Update screen of v3.8.0 in all four languages, the three settings behind it and the four calls it is served by among them.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.8.0

## Add/fix features:

- **An Update screen says what the newest release is.** The program knew how to install it and only from the command line, so an installation found out that a release existed from somewhere else or not at all.
  - The screen shows what is running beside what was released. The reading is taken on a timer rather than when the screen is drawn, so opening it costs the release API nothing and two people opening it do not make two requests; a press takes the reading now.
  - Where the release is newer, and this process is what a service registration starts, the screen offers to install it. That takes the password of the account: it replaces the executable and ends with a restart that drops every tunnel. It cannot report that it finished, because the process that would report it is the one being restarted, so it says that it started and what to look at to see that it went through.
  - Looking is on by default. Installing without anybody asking is off by default, and turning it on says what it does: a release that appears is installed and the service restarts itself, at an hour nobody chose.
  - A tag that cannot be read as three numbers is never treated as newer. An answer of newer is what starts an install on its own, and a version this does not understand is not grounds for replacing the executable of a running service.
  - The version that is running is written the way a release tag is written, with the v. The two sit side by side and are read against each other, and one written 3.8.0 beside one written v3.8.0 reads as two different things.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Installing from the screen is running this program again with `-install`, as its own process, which is the command an operator types by hand. A process that replaced its own file cannot run itself again, so the work is handed to one that has not.
- An exported configuration carries the three update settings, the automatic install among them. A file exported from an installation that has it on turns it on wherever the file is taken in.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.7.4

## Add/fix features:

- **A tunnel whose forwarded port the Host would not open says what that can be.** The row carried one line, in the English the SSH library wrote, naming neither who refused nor what to look at.
  - The Status screen now says the refusal came from the Host, and lists what makes a server refuse with the likeliest first. The order is decided on what this end knows: the server that answered the handshake, and the port it was asked for.
  - None of it is given as the cause. The refusal carries no reason with it, and the several settings behind it look identical from here, so the list is offered as a list.
  - A Host running the Windows build of OpenSSH is told nothing about privileged ports, which that platform does not have.
- **The box that takes a local port warns where it is below 1024, and stores it anyway.** Whether such a port can be opened is a fact about the far machine rather than about the value: the account the Host is registered with may be root, the Host may be Windows, and a Linux Host may be set to let ordinary accounts bind lower ports. Refusing the value would decide all of that from a screen that can see none of it.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing about which forwards are attempted has changed. A port that was refused before is refused now, and what is new is only what the screen says about it.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.7.3

## Bug fixes:

- A Host registered after another was deleted took the number the deleted one had climbed past, rather than the one it gave up. An installation where the single Host had been removed and registered again showed Host 2 with no Host 1, and the numbers went on climbing away from how many Hosts there are.
  - The number of a new Host is now one past the largest in use. A number given up from the end comes back, so removing a Host and registering another hands out the number that was just freed.
  - A gap in the middle is left as it is. The number of a Host is what the Status screen, the host key panel and the log file call it by, and one handed back in the middle would put a newly registered Host among the older ones on every screen that lists them by number, under a number an older log line already used for something else. Taken from the end, a new Host is still the last of the list.
  - An import numbers the Hosts it adds the same way, so which of the two registered a Host does not decide whether the numbers have a gap in them.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Nothing is renumbered. The Hosts an installation already has keep the numbers they were registered under, and the change is only in what the next registration is given.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.7.2

## Add/fix features:

- **The log file can be emptied from the Logs screen.** The screen could only be read, so a log that had filled with something an operator was finished with stayed there until the file rotated past it.
  - The press takes the password of the account, the way the uninstall does. What it does cannot be taken back, which is the line the uninstall is on rather than the line the restart is on: after a restart the service is running again, and after this the lines that were in the file are gone.
  - Only the file this server is writing to is emptied. The rotated files beside it are what the retention settings were set to keep, and the panel says that before it asks.
  - A wrong password is answered inside the panel, with what was typed still in front of the operator. A refusal of a password is not a session that has ended, and the login screen is not the answer to it.
  - The emptying goes through the writer that holds the file open rather than at the file. That writer carries the size it last wrote at and does not read it again while the handle is open, so a file cut under it would have its next line of any length taken for one that fills the file, and would be rotated on the spot.
- **The shortest password an account may be set to is eight bytes rather than twelve.** This is a lowering, and it reaches further than the login screen: the same bound is what a password sealing an exported configuration is held to, and that file carries the SSH credentials of every Host, is kept wherever it was put, and has no rate limit in front of it. What stands behind the login is unchanged - five failures from one address in five minutes, thirty on the account.

## Bug fixes:

- The Status screen said every tunnel that should be running was connected on an installation that has no tunnels at all. Nothing was missing and nothing was down because there was nothing, so the line read as a report on work that was never asked for. It is left off where there is no tunnel to run, and the empty list under it is what says so.
- The panel of host keys waiting for an approval kept the approve button and the count of what is ticked after the last key was answered. Approving the last one from its own row empties the list while the panel is up, and what was left was a press that sends nothing under a line counting to nothing, over a list already saying there is nothing here to approve.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.7.1

## Add/fix features:

- **The Status screen asks about a waiting host key once, not once for every tunnel.** The notice sat under every tunnel row, so a Host carrying four service ports asked the same question four times. It is a fact about the Host rather than about the tunnel.
  - What the screen carries now is one line with two counts, how many Hosts have never been approved and how many presented a key other than the one they are trusted on. The counts are of the installation and not of the page the table happens to be on, so a Host with no tunnel row yet is in them too.
  - Behind that line is a panel of the Hosts that are waiting. It pages the way the other lists do, and the fingerprint is on the row beside the tick: a tick means the fingerprint was looked at, and it cannot mean that if looking costs another click.
  - Ticks survive turning the page, and what they add up to is listed once more, with the fingerprints, before any of it is sent.
  - Ticking a whole page takes only the Hosts being approved for the first time. A key that replaced a trusted one is a server that changed, which is read one at a time, and a batch holding one of those takes the password of the account. A password that does not open the account approves none of the batch; a fingerprint that is no longer the one waiting refuses that Host and leaves the rest.

## Bug fixes:

- A wrong password on a host key approval threw the operator out to the login screen with the panel gone, saying the session had ended. It had not: the server refuses that one approval and changes nothing. Every refusal that carries a 401 but the three that ask for the account password again was read as a session that had ended, and approving a host key is a fourth.

## Notes:

- The upgrade note of v3.7.0 still holds: every Host stops until its key is approved, a stored path outside the data directory is put back to its default, and the data directory becomes 0700.
- A batch takes at most a thousand Hosts, which is ten times the largest page the panel sends. A request over that is refused rather than held, since the database this program uses serves one connection at a time and a batch of ten thousand would hold it for all of them.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.

# v3.7.0

## Add/fix features:

- **Tunnel Manager checks the host key of the SSH server it connects to.** Every connection was made with the host key ignored, so anything on the path to a Host could answer as that Host and be handed the password this program keeps encrypted in its database. A Host now reaches only the server that presents the key it is trusted on.
  - A key that has not been approved, and a key that is not the one approved before, both stop the connection rather than being retried. The Status screen says which of the two happened and opens a panel that puts the fingerprints side by side.
  - Approving a key that replaces one already trusted takes the password of the account, the way an uninstall does. Approving the first key of a Host takes only the session, since there is nothing yet to overturn.
  - The trusted key is carried in an exported configuration, so moving an installation does not drop what its Hosts are pinned to. The key waiting for an approval is not carried, being a fact about a refused connection rather than a setting.
- **A login that keeps failing is blocked.** Nothing counted a failed sign in before, so the one account could be guessed at without limit, and every guess spent a password hash on the way. The address and the account are counted apart, the refusal says how long the block has left to run, and it does not say which of the two was reached.
- The database and the log file are created 0600 under a 0700 directory. They were 0644 under 0755, so any other user of the same machine read the password hash of the account, the Hosts and the users they are registered with. Files an earlier release left wider are narrowed at startup.
- `logging_file_path` and `security_key_file` are taken only as paths inside the data directory. An operator could point the log at any path on the machine and have the service create it, append to it and read it back through the Logs screen. A path stored before this rule, or one that arrives in an imported file, is put back to its default with a warning rather than refused.
- A request is bounded in size and in time. A body is limited to one megabyte, and to thirty-two on the two import routes, whose file carries a private key for every Host; the server carries a header deadline, a read deadline and an idle deadline, which it had none of before.
- The answers carry `X-Frame-Options`, `X-Content-Type-Options` and a content security policy. HSTS is sent only where the certificate is one somebody else signed, since a self-signed one can be replaced from the Settings screen and a pin the browser is holding could not.
- The certificate this program makes for itself may sign only for the names and the addresses this machine answers to. An operator who wants the browser warning gone is told to trust it, and it was made an authority with nothing said about what it may sign, so whoever held the database file and the key file together could issue a certificate for any name at all.
- A session runs out at an absolute age rather than only at an idle deadline that every request pushed forward, so a stolen token no longer lives as long as it is used.
- `-trust-proxy-headers` marks the session cookies Secure behind a reverse proxy that terminates TLS, and lets the counters above read the forwarded address. It is off unless given, since the header is one any client can send.
- An install refuses a release that carries no `SHA256SUMS` instead of taking the download unchecked, and no download follows a redirect off https or off GitHub.
- The query log no longer carries statements on the account table. The database driver writes the bound values into the statement it hands the logger, so a failed write put the password hash in the log file, which the Logs screen reads back. It was not only tracing that wrote it; the branch for a failed query writes at the level an installation runs at.
- The initial password is only ever written into a file this program created. It went into whatever was at the path and the mode was narrowed afterwards, so a file pre-created by another local user, or a symlink, took the password before 0600 landed.
- `-uninstall -purge` says that an encryption key moved outside the data directory may still be on disk. A removal does not read the stored setting, which would mean opening the database of a service that is still running, so it cannot tell a key that was moved from one that was never made.

## Bug fixes:

- The German, Spanish, French and Portuguese screens dropped the number out of four sentences. The singular forms said "the only line read" and "every second" where the English says how many, so the count never reached the screen. The plural forms beside them had carried it all along.

## Documentation:

- The reference says what the path rule is, what `-trust-proxy-headers` does and what running behind a reverse proxy means, in all four languages.
- The design this release was built from is in `docs/design`, including the four places the build went another way than the design and why.

## Notes:

- **Every Host stops until its key is approved.** Nothing was pinned before this release, so on the first start after it every tunnel goes to "host key not approved" and the Status screen has a button for each. This is the upgrade doing what it is for, not a fault, but no tunnel carries traffic until somebody has looked at the fingerprints.
- **A stored path outside the data directory is put back to its default.** An installation that moved its log or its encryption key with an absolute path comes up on the default path instead, and says so in the log. Move the file if the old path held something worth keeping.
- **The data directory becomes 0700.** A deployment that bind mounts it from the host, as the bundled Docker Compose file does, leaves that directory readable by root alone, where it was world readable before. Anything on the host that read the log out of it needs the ownership changed.
- The container still runs as root. The bind mount takes the ownership of the host directory and Docker creates it as root when it is missing, so another user in the image cannot create the database. Moving off root is a change to the deployment as much as to the image.
- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.
- None of the twelve translations has been read by a native speaker.
- Whether a trust store other than Go's accepts a certificate that is not an authority was not established here, which is why the certificate above stays one and is limited by name instead.

# v3.6.1

## Add/fix features:

- The program draws its name and its version on the terminal as the first thing a run puts there, and the banner of the web framework it is built on is hidden. A console somebody is watching now says what started in it before any log line does.
- The line that reports the server is up names the address the listener bound, `[::]:8888` rather than the `:8888` that was asked for. The framework used to print that address and no longer prints anything, and the plain-HTTP path opens its own listener now so that it has one to ask.
- A download that fails in a way that may pass is asked for again, four times over about thirty-five seconds, before the install gives up on the release. A release published a moment ago is named by the API before the servers that carry its files have it, and those answer 502 or 504 until they do, which used to send the install straight to the executable it was started from: a report that says it worked, on the version the operator already had. A refusal is still not asked twice, since a rate limit and a release that is not there say the same thing however often they are asked.
- The log line written every time a tunnel asked for a wildcard local address is gone. It repeated the same paragraph on every such tunnel and ended by leaving the answer to the probe that follows it, which is the line worth reading.

## Notes:

- Only the Linux path of `-install` and `-uninstall` has been run. The macOS and the Windows backends are still held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce.

# v3.6.0

## Add/fix features:

- The program installs itself as a service of the machine it is run on, and takes itself back out. `-install` puts the executable in place, makes the data directory, registers the service with systemd, launchd or the Windows service control manager, and starts it; `-uninstall` stops it, takes the registration out and removes the executable. It needs root, or an administrator on Windows, and refuses without saying anything else when it does not have it.
  - What is installed is the newest release rather than the file that was run, downloaded from GitHub and checked against the `SHA256SUMS` of that release when the release carries one. A checksum that does not match stops the install. A release that cannot be reached is not a failure: the running executable is installed instead and the report names which of the two landed.
  - `-bin` and `-db` put the executable and the database somewhere other than the defaults, which are `/usr/local/bin/tunnel-manager` with `/var/lib/tunnel-manager` on Linux, the same executable path with `/Library/Application Support/tunnel-manager` on macOS, and `C:\Program Files\tunnel-manager` with `C:\ProgramData\tunnel-manager` on Windows. The data directory follows `-db`, so the key, the log and the initial password land beside the database.
  - An install over one that is already registered at the same paths writes over it and restarts the service, with the digest of the file before and after in the report. One registered at other paths is refused before anything is touched, because the executable and the database of the old one would be left with nothing naming them.
  - **An uninstall asks the running service which files it has open** rather than reading paths out of the registration. A service started without `-db`, or with the log moved on the Settings screen, tells the registration nothing, and the files that would go are then not the files that are there. Linux reads `/proc/<pid>/fd` and macOS runs `lsof`; Windows has no cheap way to list another process's handles, so there the service command line is read, which is safe because the install always writes `-db` into it.
  - The uninstall lists what it is about to remove and asks before removing any of it. `-y` answers for a script, and with no terminal to ask at it refuses rather than assuming yes. The data is kept unless `-purge` is given, and `-purge` refuses a directory that is not the one the database sits in, that is too near the root, or that the system keeps other things in.
- The program runs as a Windows service. It could not answer the service control manager before, so `sc create` would have left a service that never started. It answers now, and the install registers it to start at boot and to come back after a failure.
- Releases carry a `SHA256SUMS` file beside the binaries. A download that was cut short still looks like a binary, and the install that reads a release needs something to check it against.
- The systemd unit the install writes carries `LimitNOFILE=65535`. The process can raise its soft limit only as far as the hard one and cannot raise the hard one itself, so the limit it runs with is the one the unit sets. The unit goes to `/lib/systemd/system` and `/etc` is left to the operator, where enabling the service puts the link systemd makes.

## Bug fixes:

- A `-purge` of a directory it would refuse was refused only after the service had been stopped, its registration taken out and the executable deleted, so the refusal arrived with the installation already half gone.
- A path with a space in it was refused while the service was being registered, which is after the new executable had been written into place.

## Documentation:

- The README and the reference say how to install as a service and how to take it back out, in all four languages, including what an install does over one that is already there and which directories `-purge` refuses.
- The bundled `_scripts` are gone. The systemd unit is written by `-install`, and the script that raised the descriptor limit in `/etc/security/limits.conf` is answered by `LimitNOFILE` in that unit, which binds to this service alone rather than to every login on the machine.

## Notes:

- Only the Linux path has been run. The macOS and the Windows backends are held up by the compiler, by the vet tool for their platform, and by tests over the plist and the service configuration they produce. Neither has been installed, started or removed on the system it is for.
- The `-install` of this release is the first one that can be taken out by `-uninstall`. An installation made by an earlier release has no such flag in the binary it put in place.

# v3.5.1

## Bug fixes:

- The drawing on the manual put the service inside the frame of the machine tunnel-manager runs on, at `127.0.0.1:8080`, which taught that only a service on that same machine can be published. A service is any address tunnel-manager can open a connection to, which is usually another machine on the network it is on, so the service is drawn outside that frame now and the leg to it leaves the frame.
- The README said tunnel-manager opens the forwarded port itself. The SSH server opens it; what tunnel-manager opens is a TCP connection to it, to see whether it answers.

## Documentation:

- The notice above an export opened by saying the file holds the SSH password, the private key and the key passphrase of every Host in the clear, and named the password the file is encrypted with three sentences later. Read in that order it says the file is written in the clear, which is not what happens. It says the file is encrypted first now, and the reference says it in that order too.
- The screens, the refusals and the reference say encrypted where some of them said sealed. All thirteen translations already had one word for it, so only the English was of two minds.
- Sentences that pointed at something with that connection, that port, this end or over there, which sends the reader back a sentence to find out what is meant, name the thing instead. That is a little over a hundred sentences across the thirteen languages. The Korean also called the three columns of the Status screen 칸, which is a cell.
- The overwrite hint explained itself by the id of a row, which no screen shows; the import kinds said an import says so rather than reading the other; the atomic import said there was nothing to take apart by hand. Each says what it means in one reading now, in every language.

## Notes:

- No behaviour changed in this release. What changed is what the screens, the README and the reference say, and the drawing on the manual.
- None of the twelve translations has been read by a native speaker.

# v3.5.0

## Add/fix features:

- Which Host carries which service port is a choice now. Every Host used to carry every service port, with nothing to say otherwise, so a service port that belonged on one machine was opened on all of them. An assignment is one row, one Host and one service port, and a tunnel is built for an assignment whose Host is enabled and for nothing else.
  - The Hosts screen has a Service ports button on every row that opens a panel with the service ports listed a page at a time, ticked where the Host carries them. Saving sends what changed and not the whole set: the list is paged, so the screen never holds all of it, and sending everything it could see would have unassigned every page it could not.
  - Registering a service port assigns it to every Host, and registering a Host gives it every service port, unless the box on the form is unticked. Both boxes start ticked, so a request that says nothing about assignments is answered the way it always was.
  - `GET /api/host/:id/service-port` lists the service ports with whether the Host carries each, paged like the other lists. `PUT` takes `{"add": [ids], "remove": [ids]}` and refuses an id that is not stored or that is named on both sides, and refuses the whole change rather than half of it.
  - An export carries the assignments as `assigned_local_ports` on each Host, named by local port rather than by row id, because the id belongs to the installation the file came from. A file written before this release names none, and is read as every Host carrying every service port, which is what it meant when it was written.
- The Status screen says whether the forwarded port can be reached. A tunnel that reads `connected` is one whose SSH connection stands; which address the SSH server bound the forwarded port to is that server's decision, and with `GatewayPorts` at its default the port answers on the Host alone. Once a tunnel is up, tunnel-manager opens a TCP connection to the Host at that port itself, once, and the row carries `forward_reach` as `reachable`, `unreachable` or `unknown`, and `server_banner` as what the SSH server called itself.
  - A port that was not reached is drawn as a failure, since the tunnel is up and not usable, with what to look at under the row. The advice is by server: `GatewayPorts clientspecified` in `sshd_config` for OpenSSH, with the note that an `Include` earlier in the file wins over a line added at the bottom, and the `-a` flag for Dropbear, which has no equivalent of `clientspecified`. It names no cause. A server that bound loopback alone and a firewall on the way look the same from here, so the screen says to check both.
- The screens come in thirteen languages: English, Korean, Japanese, Chinese, Spanish, French, German, Brazilian Portuguese, Russian, Arabic, Hindi, Vietnamese and Thai. Arabic is written right to left and the whole page turns round with it.
  - A switch in the corner picks the language for that browser, and the Settings screen has `ui_default_language` for the installation. A browser that has picked one keeps it; one that has not is shown the installation's language, then the one the browser asks for, then English. The login screen is drawn before there is a session to read the settings with, so it follows the browser alone, on purpose.
  - What the server says is translated too. A refusal carries `error_code` and `error_args` beside the English `error` it always had, a line of the log carries `log_id` beside its English `msg`, and the strings an answer carries by name, such as the note after a certificate is replaced, come with a code. The log file is written in English as before; only the screen translates it, so it can still be searched and a line written by an older version shows as it was.
  - The catalogs are in the binary and nothing is fetched from the network. A language is one file of 750 keys, and a key a language lacks falls back to English.
  - The twelve translations were read once more for English carried over word for word: a tunnel that stood, a Host that carried a port, a manager that reached a server, a file that was sealed. Each language now says those things the way its own IT writing does, with the meaning held against the English sentence, and the Korean, Japanese and Chinese documents were read the same way.
- The screens have a light and a dark theme, with a switch in the corner. A browser that has never pressed it follows what the operating system prefers, and the theme is put on the page before it is first painted so a dark page is never shown light first.
- A Manual tab explains what the program is made of: how one tunnel is built, what a Host, a service port and an assignment are, how far the forwarded port is open, what the two intervals do and where a path in a setting points. The login screen opens the same text in a panel for somebody who cannot sign in yet.
  - It opens with a drawing of the two machines: the Host with its SSH server and the port opened on it, the machine tunnel-manager runs on with the service beside it, a client on the far side, and the four legs the traffic takes, numbered. Under it the same tunnel is worked through on one Host and one service port, down to the `ssh -N -R` command that would open it by hand. The drawing scrolls sideways on a phone rather than shrinking, and it is labelled in every language.

## Bug fixes:

- A timestamp in a table came out back to front on a right to left page, the clock before the date. The date and the clock are left to right text on their own, and the space between them took the direction of the page. The cell is marked left to right now, and nothing changes on a left to right page.
- Signing out left the login screen in the language of the installation rather than the one the browser asks for, which is what a new visitor sees. What was known about the installation is forgotten on the way out, and on a session that ended on its own.
- The login screen said to leave the username empty on the first sign in, and went on saying it after the setup was done. It asks the server now, through `GET /api/setup`, which answers without a session and says only whether the setup is still to come, and drops the hint once it is done. The version in the corner is written as the release names it, `v3.5.0`, in every language rather than being translated.
- The date and the clock of a line on the Logs screen touched where the column was wide enough for both, and a copy of the cell read as one run of digits. There is a space between them now, and the line still breaks there where the column is narrow.
- Three English sentences read as English only on a second try: the refusal of an account change that names nothing, the log line for a connection that sent nothing, and the Logs screen when the one line read is under the level. They say what they meant in one.
- The log line written when a wildcard local address is requested said the SSH server binds it to loopback unless `GatewayPorts` is enabled. It cannot know that, and it was wrong for `clientspecified`, which opens the port on every address as asked. It says now that the address in the line is the one that was requested, and leaves the answer to the probe that follows.

## Documentation:

- The README is an introduction: what the program does, one diagram, how to start it and where to read more. Everything it held is in `docs/reference.md`, with nothing left out, and both come in Korean, Japanese and Chinese beside English. The four are kept to the same headings, tables and API rows.

## Notes:

- An installation upgraded to this release keeps every tunnel it had. The assignment table is filled with every Host against every service port on the first start that creates it, and on no later start, so an assignment the operator removes afterwards stays removed.
- The plural form of a sentence is chosen by the rules of the language being shown, so French puts zero in the singular as it does. The catalogs carry the two forms English has, one and many, and a language whose rules name more, Russian with three and Arabic with five, falls back to many for the ones it has no sentence for; the translations of those two are written so as not to count where the form would have to change, and a sentence added under the missing form is used from then on.
- In Arabic, a path that begins with `/` is drawn with that slash on the wrong side of it. That is how a browser lays out left to right text on a right to left line, and the catalog cannot change it.
- None of the twelve translations has been read by a native speaker.

# v3.4.4

## Bug fixes:

- A refresh still replaced the status list under a finger that was resting on it. The wait watched the scroll, which is the effect, and the cause is the finger: one that is down but has not moved yet fires no scroll event, so the page read as still and the screen was replaced under the hand that was about to drag it. A touch counts as hold of the page now, and lifting it starts the quiet time rather than ending the wait, because what a phone does when a finger leaves is carry on moving.

# v3.4.3

## Bug fixes:

- A refresh still replaced the status list under a moving finger. Waiting before the request was not enough: the tick went out while the page was still, the answer took a round trip to come back, and the screen was replaced when it arrived, so a finger that came down in between met the swap anyway. Measured against that case, an answer 150ms out with the finger down at 50ms, the screen was replaced under it before and is not now. What was drawn is kept and goes up the moment the page settles, so nothing is fetched twice and no tick is lost. A draw the operator asked for is never held.

# v3.4.2

## Bug fixes:

- The built-in UI files went out with a content type and nothing else, so a browser was left to decide for itself how long they keep. A phone went on running the scripts of an earlier release after a deployment, which makes a fix measured on the server look like a fix that did nothing on the screen. They carry an entity tag now, taken from the bytes themselves, and `Cache-Control: no-cache`, which is not do not store it but ask before using what is stored: the answer is `304` with no body whenever the file has not moved.

# v3.4.1

## Bug fixes:

- A Host can be registered disabled. The create request had no `enabled` field, so one could only be registered enabled and began connecting before anyone could say otherwise; update had the field all along. A create that does not mention it is enabled, which is what every Host was before the field existed.
- Underneath that was a database default of `true` on the column. gorm leaves a field out of an insert when it holds the zero value and the column has a default, so storing `false` stored `true` even once the field was there, and naming the column in a Select does not change it. The default is gone and whatever inserts a Host now says what `enabled` is.
- The same default was reaching the import. A Host somebody had turned off on one machine arrived on another enabled and started connecting, which is the last thing an import should do by itself.

# v3.4.0

## Breaking changes:

- `GET /api/host` and `GET /api/service-port` answer with a page rather than with everything. `data` was an array and is now `{items, total, page, size}`. `GET /api/status` keeps the shape it had, with `tunnels` holding a page and `page` and `size` beside it.

## Add/fix features:

- The lists are handed out a page at a time. All three answered with every row they had, and the tunnels are one row per Host per service port, so twenty of each was four hundred rows in every answer and the screen was holding all of them to show ten.
  - `page` and `size` say which page. The size is one of 10, 20, 30, 50 and 100 rather than any number, so that one request cannot ask for the lot, and a page past the end is the last page rather than an error, because a list that shrank under a screen already looking at page nine should show page eight.
  - The counts above the status table stay counts of everything. A total that quietly became the size of a page would read as a tunnel count that dropped to ten.
  - The screens choose the size and the page above the table, and each remembers its own choice. A list short enough to fit the smallest page carries no controls at all.

## Bug fixes:

- The status screen took the reader back to the first row every five seconds. Emptying the page takes its height down to the heading, and a page shorter than where it is scrolled to is scrolled back by the browser, so the position is taken before the screen is rebuilt and put back after. A page rebuilt under a finger loses the scroll whatever the position is put back to, so a refresh that comes due while the reader is scrolling now waits until they stop rather than going ahead. It waits rather than skips: it comes as soon as the scrolling ends.

# v3.3.0

## Add/fix features:

- The configuration can be carried to another installation in a file. The Settings screen exports the tunnels (every Host and service port) and the settings of the manager, each as one line of text sealed with a password typed at the time, and imports the same. What seals it is that password and not the encryption key of the installation, because that key belongs to one machine and sending it along would only move the question of what guards it.
  - The SSH password, the private key and the passphrase of every Host are opened with the installation key on the way out and sealed again with the installation key of wherever they land. The file therefore carries them, and the export card says so rather than softening it. Measured on two installations with different keys: the file shows nothing of the key, the user or the address in the clear, and the tunnel the second one builds from it authenticates and carries traffic.
  - An import adds what is not there and reports what it skipped and why, or replaces when it is told to. It is one transaction, so a file refused halfway leaves nothing behind.
  - Imported settings are stored rather than applied. `api_port` and `api_https_enabled` decide whether the screen can be reached at all and a file from another machine carries that machine's answers, so they appear as waiting for a restart where they can be read before they take hold.
- The card at the top of the Settings screen that lists what is stored but not being run on has the Restart button in it. It used to say the restart was further down the screen and leave the reader to go and find it. It is the same press as the one down there, not a second one.

# v3.2.7

## Bug fixes:

- Last connected and Updated came apart in the middle of a timestamp. The previous release let the log break its stamp between the date and the clock by changing the class every table uses, and the other tables were relying on that class to hold them to one line, so the two halves read as two values stacked in a cell. The log has a class of its own for it now.
- The last error of a tunnel has the width of the table rather than the ninth column of it. The other eight columns are addresses, counts and a timestamp and already ask for more width than a screen has: measured against a real SSH failure, the column had 144px on a laptop with the row standing 183px tall, and 106px and 288px on a phone. It goes under the row now, and only when there is one, where the same message has 941px on a laptop and 797px on a phone, on one line either way.

# v3.2.6

## Add/fix features:

- Rotated log files are compressed by default. What it compresses is a log that has already been rotated, which nothing reads again except when something has gone wrong, and the text of a log is most of its size: the other defaults ask for five backups of 100MB, which uncompressed is half a gigabyte of a disk that has other uses. It is what a fresh installation starts on, and an installation that already has the setting stored keeps whichever way it was left.
- The Settings screen says what a stored path is read against, under both of the settings that are one, and names the directory rather than describing it. Which directory that is depends on how the service was started and cannot be seen from a browser, so the server sends it. On Windows the sentence is the Windows one: a path is absolute only when it names a drive or a share, and one beginning with a single backslash is not, so it is read against that directory like any other.

## Bug fixes:

- The Caller column of the log was narrower than what it held, and broke inside names. It was allowed to break wherever it liked, which is also what cost it the room: a run of text that may break anywhere has a smallest width of one character, and a table hands out what is left over by what each column says it needs. It breaks after a path separator now and asks for the width of the longest piece between them. Measured at 844px, a phone on its side: the column had 113px and now has 155px, with no change to how tall a row stands.

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
