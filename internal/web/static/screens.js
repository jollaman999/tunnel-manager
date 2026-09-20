"use strict";

// The screens of the UI. The key is the path under /ui/, the label is the
// catalog key the navigation is drawn from, and nav marks the screens that are
// reached from it: the
// login and the setup are left out because they are what a client without a
// session or without a finished account is sent to, not places to go on a whim.
//
// draw fetches and draws. enter, where a screen has one, is what runs once on
// arrival: it is where a screen resets what it was left in and starts a timer,
// neither of which may happen again on every redraw.
const screens = {
  status: { label: "nav.status.link", nav: true, draw: drawStatus, enter: enterStatus },
  hosts: { label: "nav.hosts.link", nav: true, draw: drawHosts, enter: enterHosts },
  "service-ports": {
    label: "nav.service-ports.link",
    nav: true,
    draw: drawServicePorts,
    enter: enterServicePorts
  },
  logs: { label: "nav.logs.link", nav: true, draw: drawLogs, enter: enterLogs },
  settings: { label: "nav.settings.link", nav: true, draw: drawSettings, enter: enterSettings },
  // The manual is the other screen that asks the server for nothing. What is on
  // it is true of every installation, so there is nothing to fetch, and that is
  // what lets the login put the same thing in a panel for somebody who has no
  // session yet.
  manual: { label: "nav.manual.link", nav: true, draw: drawManual },
  login: { draw: drawLogin },
  setup: { draw: drawSetup },
  // The screen after the uninstall. It is out of the navigation for the same
  // reason the two above are, and it is the one screen that asks the server for
  // nothing: by the time it is drawn the server is seconds away from being gone.
  uninstalled: { draw: drawUninstalled }
};

// logLevels and logFormats are what the server takes for those two settings.
// They are lists on the screen rather than boxes, so a value the server refuses
// cannot be sent at all. The lists are the ones Validate holds in
// internal/settings/settings.go, and a value added there shows up here only
// once it is added here too.
const logLevels = ["debug", "info", "warn", "error", "dpanic", "panic", "fatal"];
const logFormats = ["json", "console"];

// restartPollEverySec is how often the page asks whether the service is back
// after a restart, and restartPollLimitSec is how long it goes on asking.
//
// The limit covers what a restart is made of: the seconds the server stays up
// so that the answer arrives, up to ten seconds of draining the requests that
// are in flight, up to ten more for the reconcile loop to return, the tunnels
// coming down, and then a startup that builds every tunnel before it listens,
// where a Host that does not answer costs ten seconds of SSH timeout. Ninety
// seconds is past all of that on an installation of a handful of Hosts.
//
// There is a limit at all because a page that asks forever says nothing: the
// screen looks the same whether the service is slow to come back or is not
// coming back, and only one of those is something to act on. Past the limit the
// page says so and stops asking.
const restartPollEverySec = 1;
const restartPollLimitSec = 90;

// editingHostID and editingServicePortID say which row has the edit form open.
// Only the identifier is kept: the values in the form come from the last answer
// the list was drawn from, so an edit form never shows a row as it was several
// fetches ago.
let editingHostID = null;
let editingServicePortID = null;

// listSizes are the sizes a page of a list may be asked for in, and the first
// of them is the size a screen starts on. They are the sizes the API takes: one
// that is not on its list is refused there rather than brought into range, so a
// size added here and not there is a screen asking for a page it cannot be
// given.
const listSizes = [10, 20, 30, 50, 100];

// listPages is the page each of the three lists is on and the size it is read
// in. They are held out here for the reason the lists above the log are: the
// screens are drawn again from scratch on every refresh and after every action,
// and a draw that came back on the first page would take the operator off what
// they were reading every five seconds.
//
// Each screen has its own. The three lists are not the same length: the tunnels
// are one row per Host per service port, so they run to the product of the
// other two, and a size chosen for one list says nothing about the size wanted
// on the next.
const listPages = {
  status: { number: 1, size: listSizes[0] },
  hosts: { number: 1, size: listSizes[0] },
  "service-ports": { number: 1, size: listSizes[0] }
};

// certificateDraft is what was typed into the two PEM boxes. It is kept because
// the screen is drawn again after a registration that was refused, and a paste
// of thirty lines that is thrown away on every refusal is one the operator has
// to go and find again to correct a stray character in it.
let certificateDraft = { certPEM: "", keyPEM: "" };

// certificateProblem is why the last registration was refused, in the words the
// server used. It says which of the two boxes is wrong and what about it, which
// is the whole of what the operator has to go on.
let certificateProblem = "";

// certificateReplaceResult is what the last renewal or registration answered.
// It is kept because the screen is drawn again from the server right
// afterwards, and what the replacement said about the connections that are
// already open is not in that answer.
let certificateReplaceResult = null;

// transferDraft is what was pasted into the two import boxes. It is kept for
// the reason the certificate boxes are kept: the screen is drawn again after an
// import that was refused, and a file that has to be found and pasted a second
// time to correct the one thing that was wrong is one nobody bothers to correct.
//
// The password typed beside it is not kept. Nothing on these screens holds a
// password past the call it was typed for.
let transferDraft = { tunnels: "", settings: "" };

// transferProblem is why the last import of each kind was refused, in the words
// the server used: which of the four ways the file did not open, or which row
// of it stopped the write.
let transferProblem = { tunnels: "", settings: "" };

// transferResult is what the last import of a tunnel configuration did with
// every row of the file. It is kept because the screen is drawn again from the
// server right afterwards, and that answer says nothing about what was skipped
// or why.
let transferResult = null;

// restartInFlight says whether a restart was asked for and the page is still
// waiting for the service to answer again. The button is disabled while it is
// on, because a second press asks a server that is on its way down and puts the
// failure of that call on the screen of a restart that is going fine.
let restartInFlight = false;

// uninstallResult is what the uninstall answered: the files that went and the
// ones that could not be removed. It is kept here because the screen that shows
// it is drawn after the server has removed itself, so there is nothing left to
// ask for it. A reload finds it empty, and that is what the screen says then.
let uninstallResult = null;

// logOut ends the session and goes to the login. The cookie is dropped by the
// server, so nothing here has to be cleared.
async function logOut() {
  await apiCall("POST", "/api/logout");

  navigate("login", { text: t("login.signed-out.notice"), kind: "info" });
}

// drawLogin is the screen a client without a session lands on.
//
// One form covers both states of the account, because nothing the server
// answers before a login says which state it is in. The username is ignored
// while the account is still to be set up, so sending it empty is right then
// and sending it filled in is right afterwards.
function drawLogin() {
  const form = buildForm({
    name: "login",
    legend: t("login.form.title"),
    submitLabel: t("login.submit.button"),
    fields: [
      {
        name: "username",
        label: t("login.username.label"),
        note: t("login.username.hint")
      },
      { name: "password", label: t("login.password.label"), type: "password" }
    ],
    onSubmit: submitLogin
  });

  // The manual is reachable from here, with no session, because the state an
  // operator most needs it in is the one where nothing works yet. It opens as a
  // panel over this screen instead of moving to the screen that carries it:
  // there is no getting to that screen without signing in, and a move would
  // throw away whatever is half typed into the form above.
  const help = document.createElement("div");

  help.className = "buttons";
  help.appendChild(actionButton(t("manual.open.button"), "open-manual", openManualPanel));

  render(t("common.brand.text"), [form, help]);
}

async function submitLogin(values) {
  const data = await apiCall("POST", "/api/login", {
    username: values.username,
    password: values.password
  });

  // The account has no username and no chosen password yet, and a session that
  // is in that state is refused everywhere but at the setup.
  if (data !== null && data.setup_required) {
    navigate("setup", {
      text: t("setup.needed.notice"),
      kind: "info"
    });

    return;
  }

  navigate("status");
}

// drawSetup is where the account gets its username and its password. It is
// reached after the first sign in and by anything the server refuses with the
// setup as the reason.
function drawSetup() {
  const form = buildForm({
    name: "setup",
    legend: t("setup.form.title"),
    submitLabel: t("common.save.button"),
    fields: [
      { name: "username", label: t("setup.username.label") },
      {
        name: "password",
        label: t("setup.password.label"),
        type: "password",
        countBytes: true,
        note: t("setup.password.hint")
      },
      passwordConfirmationField("password_confirmation", t("setup.password-again.label"),
        "password")
    ],
    onSubmit: submitSetup
  });

  render(t("common.brand.text"), [form]);
}

// passwordConfirmationField is the second box a new password is typed into. It
// is checked against the box named by against, and nothing is sent until the
// two hold the same thing.
//
// What is sent is the password itself, once. The server is never handed the
// second copy: it would have nothing to learn from the same string twice, and
// the typo this box is here for is made in this browser.
// note, where a caller passes one, is what the second box says about itself. A
// password that seals a file is not caught at the next sign in but at the
// import, which is somewhere else entirely and possibly weeks later.
function passwordConfirmationField(name, label, against, note) {
  return {
    name: name,
    label: label,
    type: "password",
    check: function (value, values) {
      return checkPasswordConfirmation(value, values[against]);
    },
    note: note === undefined ? t("form.password-again.hint") : note
  };
}

async function submitSetup(values) {
  try {
    await apiCall("POST", "/api/setup", {
      username: values.username,
      password: values.password
    });
  } catch (error) {
    // The account was set up by someone else in the meantime. There is nothing
    // to do on this screen any more, and the credentials that now open the
    // account are the ones they chose.
    if (error instanceof ApiError && error.status === 409) {
      navigate("login", { text: error.message, kind: "info" });

      return;
    }

    throw error;
  }

  navigate("status", { text: t("setup.done.notice"), kind: "info" });
}

// enterStatus draws the screen and starts the refresh. The period is the one
// the reconcile loop runs at, so a tunnel that comes up shows up within a pass
// of it. The timer is stopped by showScreen when the screen is left.
//
// The page being read is not reset on the way in. It is what the operator left
// this screen on, the same as the lists above the log, and a refresh goes to
// the server for the page that is on the screen rather than for the first one.
function enterStatus() {
  const drawn = drawStatus();

  refreshTimer = window.setInterval(function () {
    refreshWhenStill(drawStatus);
  }, statusRefreshMs);

  return drawn;
}

async function drawStatus() {
  const page = listPages.status;
  const data = await apiCall("GET", "/api/status?" + pageQuery(page));

  // A refresh that was in flight while the operator left must not draw over
  // the screen they went to.
  if (currentScreen !== "status") {
    return;
  }

  takeListPage(page, data);

  // The three counts are of every tunnel there is and not of the page below
  // them. They are what says what the installation is doing, and a count that
  // followed the page would read as a tunnel count that fell to ten.
  const counts = document.createElement("div");
  counts.className = "counts";
  counts.appendChild(countBox(t("status.desired.label"), data.desired_tunnels, "desired"));
  counts.appendChild(countBox(t("status.rows.label"), data.total_tunnels, "total"));
  counts.appendChild(countBox(t("status.connected.label"), data.connected_tunnels, "connected"));

  const nodes = [counts];

  // The three counts differ for two different reasons, and the difference is
  // the whole point of showing all three. A tunnel with no row has not been
  // started at all, while a row that is not connected was started and failed.
  const missing = data.desired_tunnels - data.total_tunnels;
  if (missing > 0) {
    nodes.push(statusLine(
      t(plural(missing, "status.missing-one.notice", "status.missing-many.notice"),
        { count: missing }),
      "warning"
    ));
  }

  const down = data.total_tunnels - data.connected_tunnels;
  if (down > 0) {
    nodes.push(statusLine(
      t(plural(down, "status.down-one.notice", "status.down-many.notice"), { count: down }),
      "warning"
    ));
  }

  if (missing === 0 && down === 0) {
    nodes.push(statusLine(t("status.all-connected.notice"), "ok"));
  }

  const tunnels = data.tunnels === null || data.tunnels === undefined ? [] : data.tunnels;

  if (tunnels.length === 0) {
    nodes.push(statusLine(t("status.no-tunnels.empty"), "empty"));
  } else {
    const rows = tunnels.map(function (tunnel) {
      const cells = [
        tunnel.host_id,
        tunnel.sp_id,
        statusBadge(tunnel.status),
        tunnel.server,
        tunnel.local,
        tunnel.remote,
        reachBadge(tunnel.forward_reach),
        tunnel.retry_count,
        timeCell(tunnel.last_connected_at)
      ];

      // The last error goes under the row rather than in it. Eight columns of
      // addresses and counts already ask for more width than a screen has, and
      // what is left for a column holding a sentence was measured at 144px
      // against a row that stood 183px tall. Under the row it has the width of
      // the table, and a tunnel with nothing wrong carries no line at all.
      //
      // What is said about a forwarded port that did not answer goes there for
      // the same reason and is longer still, so the two share the space under
      // the row when a tunnel has both.
      const under = [];

      const failure = typeof tunnel.last_error === "string" ? tunnel.last_error : "";
      if (failure !== "") {
        const said = element("span", failure);

        said.className = "last-error";
        under.push(said);
      }

      const advice = reachAdvice(tunnel);
      if (advice !== null) {
        under.push(advice);
      }

      if (under.length === 0) {
        return cells;
      }

      if (under.length === 1) {
        return { cells: cells, under: under[0] };
      }

      const both = document.createElement("div");

      for (const node of under) {
        both.appendChild(node);
      }

      return { cells: cells, under: both };
    });

    const controls = pageControls(page, data.total_tunnels, drawStatus);
    if (controls !== null) {
      nodes.push(controls);
    }

    nodes.push(buildTable(
      [t("status.host.column"), t("status.service-port.column"), t("status.status.column"),
        t("status.server.column"), t("status.local.column"), t("status.remote.column"),
        t("status.port-reached.column"), t("status.retries.column"),
        t("status.last-connected.column")],
      rows,
      [0, 1, 7]
    ));
  }

  render(t("status.screen.title"), nodes);
}

// statusBadge is what a tunnel is, drawn so that the one row that is not
// working is found without reading the column. The word itself is kept and is
// whatever the server said, so a state added later still shows up; only the
// three that are known are coloured.
function statusBadge(status) {
  const known = { connected: "ok", error: "bad", reconnecting: "waiting" };
  const text = status === null || status === undefined ? "" : String(status);
  const badge = element("span", text);

  badge.className = "badge " + (known[text] === undefined ? "unknown" : known[text]);
  badge.dataset.status = text;

  return badge;
}

// reachBadge is whether the forwarded port answered a connection opened by
// tunnel-manager. The word is drawn as the server sent it, the way the status
// beside it is, so a value added later still shows up. A row that carries none,
// which is one written before the reading existed, reads as not measured rather
// than as an empty cell.
function reachBadge(reach) {
  const said = reach === null || reach === undefined ? "" : String(reach);
  const text = said === "" ? "unknown" : said;
  const badge = element("span", text);

  badge.className = "badge " + reachClass(text);
  badge.dataset.reach = text;

  return badge;
}

// reachClass is the colour of a reading. A port that was not reached is the one
// row on this screen that says connected and is not usable, so it is painted
// the way a failure is; a reading that was never taken stays grey, because
// nothing is known about it.
function reachClass(reach) {
  if (reach === "reachable") {
    return "ok";
  }

  if (reach === "unreachable") {
    return "bad";
  }

  return "unknown";
}

// sshServerKind is which SSH server the banner names. What has to be changed to
// open a forwarded port differs between servers, so this is what decides which
// of the paragraphs below is shown. A banner that names neither of the two is
// answered with "other", and nothing is then said about what to set.
function sshServerKind(banner) {
  const said = typeof banner === "string" ? banner.toLowerCase() : "";

  if (said.indexOf("openssh") !== -1) {
    return "openssh";
  }

  if (said.indexOf("dropbear") !== -1) {
    return "dropbear";
  }

  return "other";
}

// probedAddress is where the forwarded port was tried from here: the machine
// the SSH server runs on, at the port that was forwarded. The host comes out of
// the server column and the port out of the local column, which is the pair the
// server itself dials. Both are cut at their last colon, which is what
// separates a port from an address that has colons of its own. A value that
// cannot be cut that way gives nothing back, and the sentence is written
// without an address rather than with a wrong one.
function probedAddress(server, local) {
  const host = server === null || server === undefined ? "" : String(server);
  const bound = local === null || local === undefined ? "" : String(local);
  const hostEnd = host.lastIndexOf(":");
  const portStart = bound.lastIndexOf(":");

  if (hostEnd < 1 || portStart < 0 || portStart === bound.length - 1) {
    return "";
  }

  return host.slice(0, hostEnd) + ":" + bound.slice(portStart + 1);
}

// reachAdvice is what is said under a tunnel whose forwarded port did not
// answer. It is drawn only for a tunnel that is connected, because that is the
// one state the reading adds anything to: a tunnel that is not connected has
// nothing forwarded to reach, and its own column already says so.
//
// It names no cause. A port the SSH server bound to loopback alone and a port a
// firewall drops are the same silence seen from here, and saying it is the one
// sends the operator to change a machine that may not be at fault. What is said
// is where the port was not reached from, followed by the things to check, in
// the order of what the server called itself.
function reachAdvice(tunnel) {
  if (tunnel.status !== "connected" || tunnel.forward_reach !== "unreachable") {
    return null;
  }

  const banner = typeof tunnel.server_banner === "string" ? tunnel.server_banner : "";
  const kind = sshServerKind(banner);
  const tried = probedAddress(tunnel.server, tunnel.local);
  const box = document.createElement("div");

  box.className = "reach-advice";
  box.dataset.reachAdvice = kind;

  box.appendChild(element("strong", tried === ""
    ? t("status.reach-anywhere.text")
    : t("status.reach-address.text", { address: tried })));

  box.appendChild(element("p", t("status.reach-cause.text")));

  box.appendChild(element("p", banner === ""
    ? t("status.reach-no-banner.text")
    : t("status.reach-banner.text", { banner: banner })));

  if (kind === "openssh") {
    box.appendChild(element("p", t("status.reach-openssh.text")));
    box.appendChild(bulletList([
      t("status.reach-openssh-order.text"),
      t("status.reach-openssh-restart.text")
    ]));
  } else if (kind === "dropbear") {
    box.appendChild(element("p", t("status.reach-dropbear.text")));
    box.appendChild(bulletList([
      t("status.reach-dropbear-flag.text"),
      t("status.reach-dropbear-all.text")
    ]));
  } else {
    box.appendChild(element("p", t("status.reach-other.text")));
  }

  box.appendChild(element("p", t("status.reach-firewall.text")));

  return box;
}

// countBox is one of the three numbers at the top of the status screen.
function countBox(label, value, name) {
  const box = document.createElement("div");

  box.className = "count";
  box.dataset.count = name;
  box.appendChild(element("span", value === undefined ? "?" : value));
  box.appendChild(element("small", label));

  return box;
}

// statusLine is a sentence about the counts above it.
function statusLine(text, kind) {
  const line = element("p", text);

  line.className = "summary " + kind;
  line.dataset.summary = kind;

  return line;
}

// pageQuery is the page a list is asked for, as the query string it is asked
// with. It goes on the first request as well as the later ones: the server has
// a default of its own, and a request that leaves the two out is answered with
// that default rather than with what the screen is set to.
function pageQuery(page) {
  return "page=" + encodeURIComponent(page.number) + "&size=" + encodeURIComponent(page.size);
}

// takeListPage moves the screen onto the page the answer was actually given
// from, which is not always the page that was asked for. Rows are deleted while
// a screen is open, and a request for a page that has gone is answered with the
// last page rather than with an error. Left alone, the screen would go on
// asking for the page that is not there on every refresh.
function takeListPage(page, answer) {
  if (answer === null || answer === undefined) {
    return;
  }

  if (typeof answer.page === "number") {
    page.number = answer.page;
  }

  if (typeof answer.size === "number") {
    page.size = answer.size;
  }
}

// lastPageOf is how many pages of this size the rows make. A list with nothing
// in it is one empty page, which is what the server counts it as too.
function lastPageOf(total, size) {
  const pages = Math.ceil(total / size);

  return pages < 1 ? 1 : pages;
}

// pageControls is the row above a table: the size the list is read in, the way
// to the page on either side, and where in the list this page is.
//
// Nothing is drawn while there is nothing the row could do. A list shorter than
// the smallest size is one page at every size, so both buttons are dead and the
// list of sizes changes nothing, and three rows would carry a row of controls
// that only says there are three rows. It appears as soon as one of the sizes
// would split the list, which includes the case where the size in use does not:
// that is the state a screen is left in by choosing a hundred, and controls
// that took themselves away there would leave no way back to ten.
function pageControls(page, total, draw) {
  if (total <= listSizes[0]) {
    return null;
  }

  const row = document.createElement("div");

  row.className = "page-controls";

  // The same list, built by the same function as the ones above the log, so
  // that the two rows of controls are one thing to learn rather than two.
  row.appendChild(logSelect("page-size", t("list.page-size.label"), listSizes, page.size,
    function (value) {
      page.size = Number(value);
      // The rows move under the numbering when the size changes, so the page
      // that was being read is no longer a place. The first page is the one
      // page that means the same at every size.
      page.number = 1;

      return draw();
    }));

  const last = lastPageOf(total, page.size);

  const previous = actionButton(t("list.previous.button"), "page-previous", function () {
    page.number = page.number - 1;

    return draw();
  });

  previous.disabled = page.number <= 1;
  row.appendChild(previous);

  const next = actionButton(t("list.next.button"), "page-next", function () {
    page.number = page.number + 1;

    return draw();
  });

  next.disabled = page.number >= last;
  row.appendChild(next);

  // Which page this is and which rows are on it. The count of pages on its own
  // says nothing about how long the list is, and the rows are what the operator
  // is looking for: a Host at row 74 is found by the range and not by counting
  // pages of ten.
  const from = (page.number - 1) * page.size + 1;
  const to = Math.min(page.number * page.size, total);
  const where = element("span", t("list.where.text",
    { page: page.number, last: last, from: from, to: to, total: total }));

  where.className = "page-where";
  row.appendChild(where);

  return row;
}

function enterHosts() {
  editingHostID = null;

  return drawHosts();
}

async function drawHosts() {
  const page = listPages.hosts;
  const answer = await apiCall("GET", "/api/host?" + pageQuery(page));

  takeListPage(page, answer);

  // The rows of this page sit under items, and total is the whole list. The
  // page on its own cannot say how long the list is: a short last page and a
  // whole list look the same from here.
  const hosts = answer === null || answer.items === null || answer.items === undefined
    ? []
    : answer.items;
  const total = answer === null || typeof answer.total !== "number"
    ? hosts.length
    : answer.total;

  // The row being edited may have been deleted from somewhere else. The form
  // is dropped rather than left holding a host that no longer exists.
  const editing = hosts.find(function (host) {
    return host.id === editingHostID;
  });

  if (editing === undefined) {
    editingHostID = null;
  }

  const nodes = [editing === undefined ? hostCreateForm() : hostEditForm(editing)];

  if (hosts.length === 0) {
    nodes.push(statusLine(t("hosts.none.empty"), "empty"));
  } else {
    const controls = pageControls(page, total, drawHosts);
    if (controls !== null) {
      nodes.push(controls);
    }

    nodes.push(buildTable(
      [t("hosts.id.column"), t("hosts.ip.column"), t("hosts.port.column"),
        t("hosts.user.column"), t("hosts.description.column"), t("hosts.enabled.column"),
        t("hosts.updated.column"), ""],
      hosts.map(hostRow),
      [0, 2]
    ));
  }

  render(t("hosts.screen.title"), nodes);
}

function hostRow(host) {
  const buttons = document.createElement("div");

  buttons.className = "buttons";
  buttons.appendChild(actionButton(t("common.edit.button"), "host-edit-" + host.id, function () {
    editingHostID = host.id;

    return drawHosts();
  }));
  buttons.appendChild(actionButton(t("hosts.service-ports.button"), "host-service-ports-" + host.id, function () {
    return openHostServicePorts(host);
  }));
  buttons.appendChild(actionButton(
    host.enabled ? t("hosts.disable.button") : t("hosts.enable.button"),
    "host-toggle-" + host.id,
    function () {
      return toggleHost(host);
    }
  ));
  buttons.appendChild(actionButton(t("common.delete.button"), "host-delete-" + host.id, function () {
    return deleteHost(host);
  }, "danger"));

  return [
    host.id,
    host.ip,
    host.port,
    host.user,
    host.description,
    host.enabled ? t("common.yes.text") : t("common.no.text"),
    timeCell(host.updated_at),
    buttons
  ];
}

// ipField and portField are the two kinds of box that hold something the server
// has a rule about. They are built here rather than written out at each of the
// five places they appear, so that the characters a box takes and the check it
// is put through cannot drift apart between the add form and the edit form.
//
// The hint doubles as the example of the form that is wanted. The addresses in
// it are the ones set aside for documentation, so neither names a real host.
function ipField(name, label, value) {
  return {
    name: name,
    label: label,
    value: value,
    hint: t("form.ip-example.hint"),
    filter: ipCharacters,
    check: checkIP
  };
}

function portField(name, label, value) {
  return {
    name: name,
    label: label,
    value: value,
    hint: t("form.port-example.hint"),
    inputMode: "numeric",
    filter: portCharacters,
    check: checkPort
  };
}

// privateKeyField and keyPassphraseField are the key half of how a host is
// logged in to. They are built here for the same reason the IP and the port
// boxes are: the add form and the edit form have to say the same thing about
// them, and the only difference between the two is what an empty box means.
//
// The key goes in a textarea with somewhere to drop a file next to it. The file
// is read in the browser and its text is what is sent, so the key file itself
// never leaves the machine the browser runs on.
function privateKeyField(note) {
  return {
    name: "private_key",
    label: t("form.private-key.label"),
    type: "textarea",
    hint: t("form.private-key-example.hint"),
    check: checkPrivateKeyBlock,
    drop: { label: t("form.drop-key.label") },
    note: note
  };
}

function keyPassphraseField() {
  return {
    name: "key_passphrase",
    label: t("form.key-passphrase.label"),
    type: "password",
    note: t("form.key-passphrase.hint")
  };
}

// checkPrivateKeyBlock catches the paste that is plainly not a key before a
// round trip. An empty box is not a problem: a host may be registered with a
// password alone, and on the edit form an empty box keeps the stored key.
// Whether the key parses, and whether the passphrase opens it, is for the
// server to say.
function checkPrivateKeyBlock(value) {
  const text = String(value).trim();

  if (text === "") {
    return "";
  }

  if (text.indexOf("-----BEGIN") === -1) {
    return t("form.private-key.error");
  }

  return "";
}

function hostCreateForm() {
  return buildForm({
    name: "host-create",
    legend: t("hosts.add.title"),
    submitLabel: t("common.add.button"),
    fields: [
      ipField("ip", t("hosts.ip.label")),
      portField("port", t("hosts.ssh-port.label"), 22),
      { name: "user", label: t("hosts.user.label") },
      privateKeyField(t("hosts.key-add.hint")),
      keyPassphraseField(),
      {
        name: "password",
        label: t("hosts.password.label"),
        type: "password",
        note: t("hosts.password-add.hint")
      },
      { name: "description", label: t("hosts.description.label") },
      // Ticked to begin with, because a Host with no assignment runs no tunnel
      // at all and carrying everything is what the API does with a request that
      // does not mention the field. It is on the add form alone: it says what a
      // Host starts with, and what it carries after that is changed with the
      // Service ports button in its row.
      {
        name: "assign_all_service_ports",
        label: t("hosts.assign-all.label"),
        type: "checkbox",
        value: true,
        note: t("hosts.assign-all.hint")
      }
    ],
    onSubmit: createHost
  });
}

function hostEditForm(host) {
  return buildForm({
    name: "host-edit",
    legend: t("hosts.edit.title", { id: host.id }),
    submitLabel: t("common.save.button"),
    fields: [
      ipField("ip", t("hosts.ip.label"), host.ip),
      portField("port", t("hosts.ssh-port.label"), host.port),
      { name: "user", label: t("hosts.user.label"), value: host.user },
      privateKeyField(t("hosts.key-edit.hint")),
      keyPassphraseField(),
      {
        name: "password",
        label: t("hosts.password.label"),
        type: "password",
        note: t("hosts.password-edit.hint")
      },
      { name: "description", label: t("hosts.description.label"), value: host.description },
      { name: "enabled", label: t("hosts.enabled.label"), type: "checkbox", value: host.enabled }
    ],
    onSubmit: function (values) {
      return updateHost(host, values);
    },
    onCancel: function () {
      editingHostID = null;

      return drawHosts();
    }
  });
}

async function createHost(values) {
  const body = {
    ip: values.ip.trim(),
    port: asNumber(values.port),
    user: values.user.trim(),
    description: values.description,
    assign_all_service_ports: values.assign_all_service_ports
  };

  body.password = values.password;

  // The key boxes are sent only when they hold something, so that a host
  // registered with a password alone carries no empty key.
  const privateKey = values.private_key.trim();
  if (privateKey !== "") {
    body.private_key = privateKey;
    body.key_passphrase = values.key_passphrase;
  }

  await apiCall("POST", "/api/host", body);

  setNotice(t("hosts.added.notice", { ip: body.ip }), "info");

  return drawHosts();
}

async function updateHost(host, values) {
  const body = {
    ip: values.ip.trim(),
    user: values.user.trim(),
    description: values.description,
    enabled: values.enabled
  };

  const port = asNumber(values.port);
  if (port !== null) {
    body.port = port;
  }

  // An empty box means the stored password stays. The API leaves out what the
  // request does not carry, so the field is left out rather than sent empty:
  // sent empty it would be refused, and sent as anything else it would be the
  // new password.
  if (values.password !== "") {
    body.password = values.password;
  }

  // An empty key box means the stored key stays, the same way the password box
  // works. The passphrase rides with the key: the server checks the two
  // together and refuses a passphrase that arrives on its own.
  const privateKey = values.private_key.trim();
  if (privateKey !== "") {
    body.private_key = privateKey;
    body.key_passphrase = values.key_passphrase;
  }

  await apiCall("PUT", "/api/host/" + host.id, body);

  editingHostID = null;
  setNotice(t("hosts.updated.notice", { id: host.id }), "info");

  return drawHosts();
}

async function toggleHost(host) {
  await apiCall("PUT", "/api/host/" + host.id, { enabled: !host.enabled });

  setNotice(t(host.enabled ? "hosts.now-disabled.notice" : "hosts.now-enabled.notice",
    { id: host.id }), "info");

  return drawHosts();
}

async function deleteHost(host) {
  // Deleting a host takes its tunnels down with it, which is not something the
  // operator can take back with another click.
  if (!window.confirm(t("hosts.delete.confirm", { id: host.id, ip: host.ip }))) {
    return;
  }

  await apiCall("DELETE", "/api/host/" + host.id);

  if (editingHostID === host.id) {
    editingHostID = null;
  }

  setNotice(t("hosts.deleted.notice", { id: host.id }), "info");

  return drawHosts();
}

// openHostServicePorts puts up the panel that says which service ports a Host
// carries and lets them be ticked.
//
// The list is served a page at a time, so what is ticked is held in two maps
// rather than read off the boxes at the end. served is what the server said
// about each service port on the pages that were read, and wanted holds the
// ones the operator touched. A box is drawn from wanted where there is an entry
// for it and from served otherwise, which is what keeps a tick made on the
// first page while the second one is being read and after coming back.
//
// What is sent is the difference between the two. The panel knows nothing of
// the pages it has not read, so a request carrying the whole set would name
// this page alone, and every assignment outside it would be deleted by a press
// that was meant to tick one box.
async function openHostServicePorts(host) {
  // The panel has a page of its own and does not touch listPages. It is opened
  // and closed while the list behind it stays where it is, and the two lists
  // are not the same length anyway.
  const page = { number: 1, size: listSizes[0] };
  const served = {};
  const wanted = {};

  const list = document.createElement("div");

  list.className = "assign-list";
  list.dataset.list = "host-service-ports";

  // Why a save or a page was refused. It is shown inside the panel because the
  // line above the screen is behind the backdrop, where the operator who
  // pressed the button cannot read it.
  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "host-service-ports";
  problem.hidden = true;

  async function drawPage() {
    const answer = await apiCall("GET",
      "/api/host/" + host.id + "/service-port?" + pageQuery(page));

    takeListPage(page, answer);

    const items = answer === null || answer.items === null || answer.items === undefined
      ? []
      : answer.items;
    const total = answer === null || typeof answer.total !== "number"
      ? items.length
      : answer.total;

    // Only the list is built again. The panel around it is the one openModal
    // put up, and nothing here writes to #app, so a draw of the screen behind
    // the backdrop cannot take the panel down and this cannot draw over it.
    list.textContent = "";

    if (items.length === 0) {
      list.appendChild(statusLine(t("service-ports.none.empty"), "empty"));

      return;
    }

    const controls = pageControls(page, total, turnPage);
    if (controls !== null) {
      list.appendChild(controls);
    }

    for (const item of items) {
      served[item.id] = Boolean(item.assigned);

      list.appendChild(servicePortCheck(item, served, wanted));
    }
  }

  // drawn is the page the list on the screen was built from, and turnPage is
  // what the controls call. A page that could not be fetched leaves the list as
  // it was, so the numbers are put back to it: left where the press moved them,
  // the next press would step over a page that was never read.
  let drawn = { number: page.number, size: page.size };

  function turnPage() {
    return drawPage().then(function () {
      drawn = { number: page.number, size: page.size };
    }, function (error) {
      if (error instanceof Redirected) {
        throw error;
      }

      page.number = drawn.number;
      page.size = drawn.size;

      showPanelProblem(problem, error.message);
    });
  }

  // The first page is fetched before the panel goes up, so that a refusal is
  // answered with the line above the screen rather than with an empty panel.
  await drawPage();

  drawn = { number: page.number, size: page.size };

  const outcome = await openModal({
    name: "host-service-ports",
    title: t("hosts.assign.title", { id: host.id, ip: host.ip }),
    body: [
      element("p", t("hosts.assign.text")),
      problem,
      list
    ],
    buttons: [
      {
        label: t("common.save.button"),
        name: "save",
        variant: "primary",
        press: function (button, close) {
          return saveHostServicePorts(host, served, wanted, button, close, problem);
        }
      },
      { label: t("common.close.button"), name: "close" }
    ]
  });

  // A panel that was closed or dismissed changed nothing, and the list behind
  // it is the list it was opened from.
  if (outcome !== "saved" && outcome !== "unchanged") {
    return;
  }

  return drawHosts();
}

// servicePortCheck is one service port in that panel: the box, what the service
// port is, and which one it is.
//
// The row is a label with the box inside it, so the whole row is the press.
// A checkbox on its own is a target the width of a character, which is the one
// thing a list ticked on a phone cannot be.
function servicePortCheck(item, served, wanted) {
  const row = document.createElement("label");

  row.className = "assign-row";
  row.dataset.assign = String(item.id);

  const box = document.createElement("input");

  box.type = "checkbox";
  box.dataset.field = "assign-" + item.id;
  box.checked = item.id in wanted ? wanted[item.id] : served[item.id];
  box.addEventListener("change", function () {
    wanted[item.id] = box.checked;
  });

  const text = document.createElement("span");

  text.className = "assign-text";
  text.appendChild(element("span", t("hosts.assign-row.text",
    { ip: item.service_ip, port: item.service_port, local: item.local_port })));

  const description = item.description === undefined || item.description === null
    ? ""
    : String(item.description);
  const said = element("small", description === ""
    ? t("hosts.assign-said.text", { id: item.id })
    : t("hosts.assign-said-description.text", { id: item.id, description: description }));

  said.className = "assign-said";
  text.appendChild(said);

  row.appendChild(box);
  row.appendChild(text);

  return row;
}

// saveHostServicePorts sends what was ticked, as the change it is.
//
// A panel that was not changed sends nothing at all. The request would carry
// two empty lists, write no row and answer that it wrote none, so the round
// trip decides nothing; the panel closes and the list behind it is drawn again,
// which is what a save does.
//
// A refusal leaves the panel up with the ticks in it. They are the operator's
// work, several pages of it, and a panel that closed on a refusal would throw
// that away along with the chance to put right whatever was wrong.
async function saveHostServicePorts(host, served, wanted, button, close, problem) {
  const add = [];
  const remove = [];

  for (const key of Object.keys(wanted)) {
    if (wanted[key] === served[key]) {
      continue;
    }

    if (wanted[key]) {
      add.push(Number(key));
    } else {
      remove.push(Number(key));
    }
  }

  if (add.length === 0 && remove.length === 0) {
    setNotice(t("hosts.assign-unchanged.notice", { id: host.id }), "info");

    close("unchanged");

    return;
  }

  // The button is held down for the whole call. The panel stays up while it is
  // in flight, which is an invitation to press again, and the second press
  // would send the same change a second time.
  button.disabled = true;
  problem.hidden = true;

  try {
    const answer = await apiCall("PUT", "/api/host/" + host.id + "/service-port",
      { add: add, remove: remove });

    // The counts come from the answer, because they are counted over the rows
    // that were written and not over the request: a service port that was
    // already assigned is asked for again without a row being written.
    const added = answer === null || typeof answer.added !== "number" ? add.length : answer.added;
    const removed = answer === null || typeof answer.removed !== "number"
      ? remove.length
      : answer.removed;

    setNotice(t(plural(added, "hosts.assign-saved-one.notice", "hosts.assign-saved-many.notice"),
      { id: host.id, added: added, removed: removed }), "info");

    close("saved");
  } catch (error) {
    if (error instanceof Redirected) {
      // The session ended and the page is on its way to the login. The panel
      // goes with the screen it was opened from.
      close(null);

      throw error;
    }

    showPanelProblem(problem, error.message);
  } finally {
    button.disabled = false;
  }
}

// showPanelProblem puts a refusal inside the panel. It is brought into view
// because the list above it may be scrolled far from the top, and the button
// that was pressed sits at the bottom of the panel: the line would otherwise be
// written somewhere the operator is not looking.
function showPanelProblem(problem, message) {
  problem.textContent = message;
  problem.hidden = false;
  problem.scrollIntoView({ block: "nearest" });
}

function enterServicePorts() {
  editingServicePortID = null;

  return drawServicePorts();
}

async function drawServicePorts() {
  const page = listPages["service-ports"];
  const answer = await apiCall("GET", "/api/service-port?" + pageQuery(page));

  takeListPage(page, answer);

  const ports = answer === null || answer.items === null || answer.items === undefined
    ? []
    : answer.items;
  const total = answer === null || typeof answer.total !== "number"
    ? ports.length
    : answer.total;

  const editing = ports.find(function (port) {
    return port.id === editingServicePortID;
  });

  if (editing === undefined) {
    editingServicePortID = null;
  }

  const nodes = [
    editing === undefined ? servicePortCreateForm() : servicePortEditForm(editing)
  ];

  if (ports.length === 0) {
    nodes.push(statusLine(t("service-ports.none.empty"), "empty"));
  } else {
    const controls = pageControls(page, total, drawServicePorts);
    if (controls !== null) {
      nodes.push(controls);
    }

    nodes.push(buildTable(
      [t("service-ports.id.column"), t("service-ports.service-ip.column"),
        t("service-ports.service-port.column"), t("service-ports.local-port.column"),
        t("service-ports.description.column"), t("service-ports.updated.column"), ""],
      ports.map(servicePortRow),
      [0, 2, 3]
    ));
  }

  render(t("service-ports.screen.title"), nodes);
}

function servicePortRow(port) {
  const buttons = document.createElement("div");

  buttons.className = "buttons";
  buttons.appendChild(actionButton(t("common.edit.button"), "service-port-edit-" + port.id, function () {
    editingServicePortID = port.id;

    return drawServicePorts();
  }));
  buttons.appendChild(actionButton(t("common.delete.button"), "service-port-delete-" + port.id, function () {
    return deleteServicePort(port);
  }, "danger"));

  return [
    port.id,
    port.service_ip,
    port.service_port,
    port.local_port,
    port.description,
    timeCell(port.updated_at),
    buttons
  ];
}

function servicePortCreateForm() {
  return buildForm({
    name: "service-port-create",
    legend: t("service-ports.add.title"),
    submitLabel: t("common.add.button"),
    fields: [
      ipField("service_ip", t("service-ports.service-ip.label")),
      portField("service_port", t("service-ports.service-port.label")),
      portField("local_port", t("service-ports.local-port.label")),
      { name: "description", label: t("service-ports.description.label") },
      // The other half of the pair on the host form, ticked to begin with for
      // the same reason, and on the add form alone for the same reason.
      {
        name: "assign_to_all_hosts",
        label: t("service-ports.assign-all.label"),
        type: "checkbox",
        value: true,
        note: t("service-ports.assign-all.hint")
      }
    ],
    onSubmit: createServicePort
  });
}

function servicePortEditForm(port) {
  return buildForm({
    name: "service-port-edit",
    legend: t("service-ports.edit.title", { id: port.id }),
    submitLabel: t("common.save.button"),
    fields: [
      ipField("service_ip", t("service-ports.service-ip.label"), port.service_ip),
      portField("service_port", t("service-ports.service-port.label"), port.service_port),
      portField("local_port", t("service-ports.local-port.label"), port.local_port),
      { name: "description", label: t("service-ports.description.label"), value: port.description }
    ],
    onSubmit: function (values) {
      return updateServicePort(port, values);
    },
    onCancel: function () {
      editingServicePortID = null;

      return drawServicePorts();
    }
  });
}

// servicePortBody is the same shape for adding and for editing, because the
// update takes the whole record: a field left out of it is not kept, it is
// refused. So the edit form is filled with what is stored and sends all of it
// back, changed or not.
function servicePortBody(values) {
  return {
    service_ip: values.service_ip.trim(),
    service_port: asNumber(values.service_port),
    local_port: asNumber(values.local_port),
    description: values.description
  };
}

async function createServicePort(values) {
  const body = servicePortBody(values);

  // The assignment rides on the registration alone, which is why it is added
  // here rather than in servicePortBody: the edit form sends that same body,
  // and the field means nothing to a service port that is already stored.
  body.assign_to_all_hosts = values.assign_to_all_hosts;

  await apiCall("POST", "/api/service-port", body);

  setNotice(t("service-ports.added.notice",
    { ip: body.service_ip, port: body.service_port }), "info");

  return drawServicePorts();
}

async function updateServicePort(port, values) {
  await apiCall("PUT", "/api/service-port/" + port.id, servicePortBody(values));

  editingServicePortID = null;
  setNotice(t("service-ports.updated.notice", { id: port.id }), "info");

  return drawServicePorts();
}

async function deleteServicePort(port) {
  // The tunnels that carry this service port go down with it.
  if (!window.confirm(t("service-ports.delete.confirm",
      { id: port.id, ip: port.service_ip, port: port.service_port }))) {
    return;
  }

  await apiCall("DELETE", "/api/service-port/" + port.id);

  if (editingServicePortID === port.id) {
    editingServicePortID = null;
  }

  setNotice(t("service-ports.deleted.notice", { id: port.id }), "info");

  return drawServicePorts();
}

// logLineCounts are the numbers of lines the screen offers to ask for. The
// largest is the bound the server puts on one request (logsMaxLines in
// internal/api/logs.go): a larger number would be cut down there anyway, and a
// list offering it would be promising what it does not hand back.
const logLineCounts = ["100", "200", "500", "1000", "2000"];

// logLevelAll is the setting of the level list that hides nothing. It is not a
// level, so it cannot collide with one the server writes.
const logLevelAll = "all";

// logLineCount and logLevelFilter are what the two lists above the log are set
// to. They are held out here because the screen is drawn again from scratch
// every few seconds, and a refresh has to come back the way the operator left
// it. They are kept across visits on purpose: they are a view of the log rather
// than state about what the server holds.
let logLineCount = "200";
let logLevelFilter = logLevelAll;

// logAutoRefresh is what the checkbox is set to. The timer runs for as long as
// the screen is on either way and is cleared by leaving it; what the box
// decides is whether a tick fetches anything. Starting and stopping the timer
// from the box instead would put a second place in charge of a timer that only
// showScreen may clear, and a box left off while the screen was left would
// leave the timer running.
let logAutoRefresh = true;

// enterLogs draws the screen and starts the refresh, on the period the status
// screen runs at. The timer is stopped by showScreen when the screen is left,
// so no tick outlives the screen it was started on and a second visit does not
// leave a second timer behind it.
function enterLogs() {
  const drawn = drawLogs();

  refreshTimer = window.setInterval(function () {
    if (!logAutoRefresh) {
      return;
    }

    refreshWhenStill(drawLogs);
  }, statusRefreshMs);

  return drawn;
}

async function drawLogs() {
  let answer = null;
  let problem = null;

  try {
    answer = await apiCall("GET", "/api/logs?lines=" + encodeURIComponent(logLineCount));
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    // A log that cannot be read is drawn as the reason it cannot be read, on
    // the screen itself. Thrown on, it would become the line above the screen
    // and the screen would be drawn again, which on this screen means asking
    // again and failing again. The commonest reason is not a fault either: a
    // server that could not open its log file writes to the console only, and
    // this screen is the one place that can say so.
    problem = error.message;
  }

  // A refresh that was in flight while the operator left must not draw over the
  // screen they went to.
  if (currentScreen !== "logs") {
    return;
  }

  const nodes = [logControls(), logScope()];

  if (problem !== null) {
    nodes.push(statusLine(problem, "warning"));

    render(t("logs.screen.title"), nodes);

    return;
  }

  const lines = answer === null || answer.lines === null || answer.lines === undefined
    ? []
    : answer.lines;

  const shown = lines.filter(keepLogLine);

  nodes.push(logSummary(answer, lines.length, shown.length));

  if (answer.capped) {
    nodes.push(statusLine(t("logs.capped.notice", { lines: logLineCount }), "warning"));
  }

  if (shown.length === 0) {
    nodes.push(statusLine(
      lines.length === 0
        ? t("logs.empty.empty")
        : t(plural(lines.length, "logs.none-at-level-one.empty", "logs.none-at-level-many.empty"),
          { count: lines.length, level: logLevelFilter }),
      "empty"
    ));
  } else {
    // The server hands the lines over in the order they are in the file, oldest
    // first, and they are turned around here. The newest line is what the
    // screen is opened for, and at the top it is in the same place after every
    // refresh instead of moving down as the log grows.
    const table = buildTable([t("logs.time.column"), t("logs.level.column"),
      t("logs.caller.column"), t("logs.message.column")], shown.reverse().map(logRow));

    // Marked so that a narrow screen can lay these rows out as blocks. Four
    // columns across a phone leave the message a column a few words wide, and
    // a log is read for its messages.
    //
    // buildTable hands back the scroller the table sits in, not the table, and
    // the class is added to what it carries already rather than put in its
    // place: table-scroll is what keeps a wide table from taking the page
    // sideways with it, and writing over it would cost that.
    table.classList.add("log-table");
    nodes.push(table);
  }

  render(t("logs.screen.title"), nodes);
}

// keepLogLine decides whether one line passes the level filter.
//
// A line whose level is not one the list knows is kept whatever the filter is
// set to. That covers the lines the server could not parse, which carry no
// level at all: they are the ones most likely to be what the screen was opened
// for, and a filter that hid them would hide exactly what nothing else reports.
function keepLogLine(line) {
  if (logLevelFilter === logLevelAll) {
    return true;
  }

  const rank = logLevels.indexOf(line.level);
  if (rank === -1) {
    return true;
  }

  return rank >= logLevels.indexOf(logLevelFilter);
}

// logRow is one line of the file as a row of the table.
function logRow(line) {
  return [logTimeCell(line.time), logLevelBadge(line), logCallerCell(line.caller),
    logMessageCell(line)];
}

// logTimeCell is the timestamp as the line carries it. It is not reformatted:
// the logger writes it in the time zone of the server, and rewriting it in the
// zone of the browser would put a time on the screen that is in no log file and
// cannot be searched for with grep.
// logTimeCell is when the line was written. The date and the time of day are
// put on two lines rather than one.
//
// Written out in full on one line it is twenty-eight characters that may not
// break, and it takes 220px of a table that has three other columns to fit. At
// the width of a phone held sideways that left the message 256px, and the
// message is what the screen is for. Stacked, the same column asks for about
// half of that, and the row is no taller for it: a row whose message runs to
// two lines has the room already, and the date is the half of this that is the
// same on every line anyway.
function logTimeCell(value) {
  const node = document.createElement("span");

  // Its own class, not the one the other tables use. Theirs holds a timestamp
  // to one line on purpose, because broken at the space between the date and
  // the clock the two halves read as two values stacked in a cell. This one is
  // meant to break, and only between those two halves.
  node.className = "log-stamp";

  const text = value === null || value === undefined ? "" : String(value);
  const split = text.indexOf("T");

  // Anything that is not the shape this writes is left as it stands. A line
  // that could not be parsed carries whatever it carried.
  if (split === -1) {
    node.textContent = text;

    return node;
  }

  node.appendChild(element("span", text.slice(0, split)));
  node.appendChild(element("span", text.slice(split + 1)));

  return node;
}

// logLevelBadge is the level of a line, coloured so that the one line that is
// not routine is found without reading the column. A line the server could not
// parse says that instead of being given a level it never carried.
function logLevelBadge(line) {
  const colours = {
    debug: "unknown",
    info: "note",
    warn: "waiting",
    error: "bad",
    dpanic: "bad",
    panic: "bad",
    fatal: "bad"
  };
  const text = line.parsed && line.level !== "" ? line.level : "raw";
  const badge = element("span", text);

  badge.className = "badge " + (colours[text] === undefined ? "unknown" : colours[text]);
  badge.dataset.level = text;

  return badge;
}

// logCallerCell is where the line was written. It wraps rather than being held
// on one line, so that a long package path does not decide how wide the table
// is on a narrow screen.
// logCallerCell is the file and line the entry was written from. It is allowed
// to break after a path separator and nowhere else.
//
// Left to break wherever it liked it broke inside names, so a caller read as
// two words that are not words. It also cost the column its width: a run of
// text that may break anywhere has a smallest width of one character, and a
// table hands out what is left over by what each column says it needs, so the
// caller said it needed almost nothing and was given that.
function logCallerCell(value) {
  const node = document.createElement("span");

  node.className = "log-caller";

  const text = value === null || value === undefined ? "" : String(value);
  const parts = text.split("/");

  for (let i = 0; i < parts.length; i += 1) {
    const last = i === parts.length - 1;

    node.appendChild(element("span", last ? parts[i] : parts[i] + "/"));

    if (!last) {
      node.appendChild(document.createElement("wbr"));
    }
  }

  return node;
}

// logMessageCell is what the line said, with whatever fields it carried under
// it. A line the server could not parse is shown as it stands in the file.
//
// Everything here is set as text. A log line holds whatever was logged, which
// includes what a remote host answered with, so a line carrying markup has to
// be read as characters instead of turning into elements.
function logMessageCell(line) {
  const cell = document.createElement("div");

  cell.className = "log-message";
  cell.appendChild(element("span", line.parsed ? line.message : line.raw));

  if (typeof line.extra === "string" && line.extra !== "") {
    const extra = element("small", line.extra);

    extra.className = "log-extra";
    cell.appendChild(extra);
  }

  return cell;
}

// logControls is the row of lists above the log. They act as they are changed
// rather than through a Save, because nothing they change is stored anywhere:
// the count is what the next fetch asks for, and the level is applied to what
// came back.
function logControls() {
  const row = document.createElement("div");

  row.className = "log-controls";

  row.appendChild(logSelect("log-lines", t("logs.lines.label"), logLineCounts, logLineCount,
    function (value) {
      logLineCount = value;

      return drawLogs();
    }));

  row.appendChild(logSelect("log-level", t("logs.level.label"),
    [{ value: logLevelAll, text: t("logs.level-all.option") }].concat(logLevels),
    logLevelFilter, function (value) {
      logLevelFilter = value;

      return drawLogs();
    }));

  const auto = document.createElement("label");
  const box = document.createElement("input");

  box.type = "checkbox";
  box.checked = logAutoRefresh;
  box.dataset.field = "log-auto";
  box.addEventListener("change", function () {
    logAutoRefresh = box.checked;
  });

  auto.appendChild(box);
  auto.appendChild(element("span", t("logs.auto.label", { seconds: statusRefreshMs / 1000 })));
  row.appendChild(auto);

  row.appendChild(actionButton(t("logs.refresh.button"), "log-refresh", drawLogs));

  return row;
}

// logSelect is one list of the row above, with the label that says what it is.
function logSelect(name, label, options, value, onChange) {
  const wrap = document.createElement("label");
  const select = listControl({ options: options, value: value });

  select.dataset.field = name;
  select.addEventListener("change", function () {
    run(function () {
      return onChange(select.value);
    });
  });

  wrap.appendChild(element("span", label));
  wrap.appendChild(select);

  return wrap;
}

// logScope says what this screen does not show. The server reads the file the
// logs are going into now and nothing else, so a line written before the last
// rotation is not here, and without this nothing would say where it went.
function logScope() {
  const note = element("p", t("logs.scope.text"));

  note.className = "log-scope";

  return note;
}

// logSummary says what was read to draw the table.
//
// The two sizes are in it because they are what shows that the whole file is
// not being pulled across: the log is allowed to reach a hundred megabytes
// before it rotates, and what was read to fill this screen is the end of it.
function logSummary(answer, read, shown) {
  const key = shown === read
    ? plural(shown, "logs.summary-one.empty", "logs.summary-many.empty")
    : plural(read, "logs.summary-of-one.empty", "logs.summary-of-many.empty");

  return statusLine(t(key, {
    shown: shown,
    total: read,
    read: formatBytes(answer.read),
    size: formatBytes(answer.size),
    path: answer.path
  }), "empty");
}

function enterSettings() {
  certificateDraft = { certPEM: "", keyPEM: "" };
  certificateProblem = "";
  certificateReplaceResult = null;
  transferDraft = { tunnels: "", settings: "" };
  transferProblem = { tunnels: "", settings: "" };
  transferResult = null;

  return drawSettings();
}

async function drawSettings() {
  const set = await apiCall("GET", "/api/settings");
  const certificate = await readCertificate();
  const account = await readAccount();
  const restart = await readRestart();

  const nodes = [];

  // What is stored but not being run on sits above the form, because it is
  // what has to be acted on before anything below it takes hold. It comes from
  // the read rather than from the last save, so it is here whenever the screen
  // is opened and by whoever opens it.
  // The address the service comes back at is worked out once and handed to
  // both cards that offer the restart, so the two cannot say different things
  // about where this page goes afterwards.
  const newAddress = addressAfterRestart(set);

  const pending = settingsPending(set, restart, newAddress);
  if (pending !== null) {
    nodes.push(pending);
  }

  nodes.push(settingsForm(set));
  nodes.push(certificateCard(set, certificate));
  nodes.push(certificateForm());
  nodes.push(accountCard(account));
  nodes.push(transferCard());
  nodes.push(exportTunnelsForm());
  nodes.push(importTunnelsForm());
  nodes.push(exportSettingsForm());
  nodes.push(importSettingsForm());
  nodes.push(settingsRescue());
  nodes.push(settingsRestart(restart, newAddress));
  nodes.push(settingsDangerZone());

  render(t("settings.screen.title"), nodes);
}

// readCertificate fetches what is being served over TLS, and turns a refusal
// into something to show rather than letting it take the screen down.
//
// The one refusal that is expected is the server that came up in the clear:
// there is no certificate then, and the toggle that decides it is on this very
// screen. A Settings screen that would not draw at all in that state is one the
// operator cannot use to turn HTTPS back on.
async function readCertificate() {
  try {
    return { view: await apiCall("GET", "/api/certificate"), problem: "" };
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    return { view: null, problem: error.message };
  }
}

// readAccount asks what the account is called. A refusal is turned into
// something to show for the same reason the two calls around it do it: the name
// is what the card says above the boxes, and a screen that would not draw at
// all without it is one the operator cannot use to change anything else either.
async function readAccount() {
  try {
    return { view: await apiCall("GET", "/api/account"), problem: "" };
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    return { view: null, problem: error.message };
  }
}

// readRestart asks what a restart would do here, which is what the card below
// puts to the operator before anything is pressed. A refusal is turned into
// something to show rather than taking the whole screen down over the one card
// that is not the reason anybody opened it.
async function readRestart() {
  try {
    return { view: await apiCall("GET", "/api/restart"), problem: "" };
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    return { view: null, problem: error.message };
  }
}

// settingsForm is every setting that is stored. The boxes are checked here
// against the rules the server holds, so a value it would refuse is reported
// next to the box it was typed in rather than after a round trip. A value that
// gets past this is still checked by the server: this form is a convenience,
// not the rule.
// pathNote says how a stored path is read, and names the directory it is read
// against rather than describing it: which directory that is depends on how the
// service was started, and an operator looking at a browser cannot see it.
//
// The rule is the platform's own idea of an absolute path, so the sentence
// about it has to be the platform's too. On Windows a path is absolute only
// when it names a drive, and one that begins with a separator alone is not:
// it is joined to the directory below like any other, which is the surprise
// worth naming.
function pathNote(set, whatItIs) {
  const dir = set === null || set === undefined || typeof set.install_dir !== "string"
    ? "" : set.install_dir;

  if (dir === "") {
    return t("settings.path-base.hint", { what: whatItIs });
  }

  // A drive letter and a colon is what Windows calls the start of an absolute
  // path, and it is also how this screen can tell which platform it is looking
  // at without being told.
  const windows = /^[A-Za-z]:[\\/]/.test(dir);

  if (windows) {
    return t("settings.path-windows.hint", { what: whatItIs, dir: dir });
  }

  return t("settings.path-unix.hint", { what: whatItIs, dir: dir });
}

function settingsForm(set) {
  return buildForm({
    name: "settings",
    legend: t("settings.form.title"),
    submitLabel: t("common.save.button"),
    fields: [
      settingsField(portField("api_port", t("settings.api-port.label"), set.api_port),
        t("settings.api-port.hint")),
      settingsField(secondsField("monitoring_interval_sec", t("settings.monitoring.label"),
        set.monitoring_interval_sec), t("settings.monitoring.hint")),
      settingsField(secondsField("reconcile_interval_sec", t("settings.reconcile.label"),
        set.reconcile_interval_sec), t("settings.reconcile.hint")),
      {
        name: "security_key_file",
        label: t("settings.key-file.label"),
        value: set.security_key_file,
        check: checkPath,
        note: pathNote(set, t("settings.key-file.hint"))
      },
      {
        name: "logging_level",
        label: t("settings.log-level.label"),
        value: set.logging_level,
        options: logLevels,
        note: t("settings.log-level.hint")
      },
      {
        name: "logging_format",
        label: t("settings.log-format.label"),
        value: set.logging_format,
        options: logFormats,
        note: t("settings.next-start.hint")
      },
      settingsField(
        { name: "logging_file_path", label: t("settings.log-file.label"),
          value: set.logging_file_path, check: checkPath },
        pathNote(set, t("settings.log-file.hint"))
      ),
      settingsField(countField("logging_file_max_size", t("settings.log-size.label"),
        set.logging_file_max_size), t("settings.next-start.hint")),
      settingsField(countField("logging_file_max_backups", t("settings.log-backups.label"),
        set.logging_file_max_backups), t("settings.next-start.hint")),
      settingsField(countField("logging_file_max_age", t("settings.log-age.label"),
        set.logging_file_max_age), t("settings.next-start.hint")),
      {
        name: "logging_file_compress",
        label: t("settings.log-compress.label"),
        type: "checkbox",
        value: set.logging_file_compress,
        note: t("settings.next-start.hint")
      }
    ],
    onSubmit: saveSettings
  });
}

// settingsField puts the note on a field built by one of the helpers above. The
// helpers are shared with the other screens, where there is nothing to say
// about when a value takes hold.
function settingsField(field, note) {
  field.note = note;

  return field;
}

// secondsField and countField are the two kinds of number that are not a port.
// A period has to be at least one second, while the numbers the log rotation is
// held to may be zero, which is how it is told to keep no bound.
function secondsField(name, label, value) {
  return {
    name: name,
    label: label,
    value: value,
    hint: t("form.seconds-example.hint"),
    inputMode: "numeric",
    filter: portCharacters,
    check: checkSeconds
  };
}

function countField(name, label, value) {
  return {
    name: name,
    label: label,
    value: value,
    hint: t("form.count-example.hint"),
    inputMode: "numeric",
    filter: portCharacters,
    check: checkCount
  };
}

async function saveSettings(values) {
  const body = {
    api_port: asNumber(values.api_port),
    monitoring_interval_sec: asNumber(values.monitoring_interval_sec),
    reconcile_interval_sec: asNumber(values.reconcile_interval_sec),
    security_key_file: values.security_key_file.trim(),
    logging_level: values.logging_level,
    logging_format: values.logging_format,
    logging_file_path: values.logging_file_path.trim(),
    logging_file_max_size: asNumber(values.logging_file_max_size),
    logging_file_max_backups: asNumber(values.logging_file_max_backups),
    logging_file_max_age: asNumber(values.logging_file_max_age),
    logging_file_compress: values.logging_file_compress
  };

  const data = await apiCall("PUT", "/api/settings", body);

  const changes = data === null || data.changes === null || data.changes === undefined
    ? []
    : data.changes;

  if (changes.length === 0) {
    setNotice(t("settings.saved-nothing.notice"), "info");
  } else if (data.restart_required) {
    setNotice(t("settings.saved-next-start.notice"), "info");
  } else {
    setNotice(t("settings.saved.notice"), "info");
  }

  return drawSettings();
}

// settingsPending is what is stored with a value this service is not running
// on. A save that stored one of those leaves work behind, and this is what says
// so until it is done.
//
// It is drawn from the read rather than from the answer to the save. The server
// works it out by holding the settings it read when it started against the ones
// that are stored, so it is here on a screen opened an hour later and in a
// second browser, and a restart empties it without anything being cleared: the
// service comes back running on what is stored.
//
// Whether a setting waits for a restart at all is the server's to say. It is
// the side that puts a value into place, so a screen that decided for itself
// would go on listing a setting the server had started taking on.
//
// The restart is offered here as well as in its own card further down. This is
// the card that says there is work left, and it is read at the top of a screen
// whose bottom is several cards away: pointing at a button somewhere below is
// asking the operator to go and find the one press this card is about. It is
// not a second restart. The press calls submitRestart with what the card below
// was handed, so the question, the address the page moves to afterwards and the
// waiting are one piece of code in one place.
function settingsPending(set, restart, newAddress) {
  const pending = set === null || set === undefined ||
    set.pending_restart === null || set.pending_restart === undefined
    ? []
    : set.pending_restart;

  if (pending.length === 0) {
    return null;
  }

  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "settings-pending";
  card.appendChild(element("h2", t("settings.pending.title")));
  card.appendChild(element("p", t("settings.pending.text")));
  card.appendChild(buildTable(
    [t("settings.pending-name.column"), t("settings.pending-running.column"),
      t("settings.pending-stored.column")],
    pending.map(function (item) {
      return [item.name, item.running, item.stored];
    })
  ));
  // What a restart would do here could not be read, and the card below does not
  // offer the press for that reason: the two cases it decides between are not
  // the same press at all. This button is that press, so it is left out for the
  // same reason, and the card says where the restart is instead.
  if (restart === undefined || restart === null || restart.view === null) {
    card.appendChild(statusLine(t("settings.pending-where.notice"), "warning"));

    return card;
  }

  card.appendChild(statusLine(t("settings.pending-restart.notice"), "warning"));

  const buttons = document.createElement("div");
  buttons.className = "buttons";

  const button = actionButton(t("settings.restart.button"), "settings-pending-restart", function () {
    return submitRestart(restart.view, button, newAddress);
  });

  button.disabled = restartInFlight;

  buttons.appendChild(button);
  card.appendChild(buttons);

  return card;
}

// certificateCard is what is being served over TLS right now, and the two
// things that change it: the switch that decides whether TLS is used at all,
// and the button that makes another certificate.
//
// Nothing here is drawn as markup. Every value comes from a certificate that
// somebody else may have issued, and a subject is a field an issuer fills in.
function certificateCard(set, certificate) {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "certificate";
  card.appendChild(element("h2", t("certificate.card.title")));
  card.appendChild(httpsSwitch(set));

  // What the last replacement said sits above the certificate it produced,
  // because it is the answer to the press and the table below it is what the
  // server is serving either way.
  if (certificateReplaceResult !== null) {
    card.appendChild(certificateReplacement(certificateReplaceResult));
  }

  if (certificate.view === null) {
    card.appendChild(statusLine(certificate.problem, "warning"));

    return card;
  }

  const view = certificate.view;
  const hosts = view.hosts === null || view.hosts === undefined ? [] : view.hosts;

  card.appendChild(buildTable([t("certificate.what.column"), t("certificate.value.column")], [
    [t("certificate.fingerprint.label"), fingerprintValue(view.fingerprint_sha256)],
    [t("certificate.subject.label"), view.subject],
    [t("certificate.issuer.label"), view.issuer],
    [t("certificate.signed-by.label"), view.self_signed
      ? t("certificate.self-signed.text")
      : t("certificate.other-signed.text")],
    [t("certificate.covers.label"),
      hosts.length === 0 ? t("certificate.no-hosts.text") : hosts.join(", ")],
    [t("certificate.valid-from.label"), formatTime(view.not_before)],
    [t("certificate.valid-until.label"), formatTime(view.not_after)]
  ]));

  card.appendChild(certificateValidity(view));
  card.appendChild(certificatePEM(view));

  const buttons = document.createElement("div");
  buttons.className = "buttons";
  buttons.appendChild(actionButton(t("certificate.renew.button"), "certificate-renew",
    renewCertificate));

  card.appendChild(buttons);

  return card;
}

// certificatePEM shows the certificate the server is serving, so that it can be
// taken into a trust store from the browser that is already looking at it. A
// self-signed certificate is warned about until some machine trusts it, and
// without this the only copy is in the database.
//
// The private key is not here and is never sent to this screen. What a client
// receives during every handshake is the certificate alone.
//
// It starts folded. It is a wall of base64 that is read once and never again,
// and the rows above are what the screen is for.
function certificatePEM(view) {
  const box = document.createElement("details");
  box.className = "pem-box";

  const label = document.createElement("summary");
  label.textContent = t("certificate.pem.label");
  box.appendChild(label);

  const note = document.createElement("p");
  note.className = "note";
  note.textContent = t("certificate.pem.hint");
  box.appendChild(note);

  const text = document.createElement("pre");
  text.className = "pem";
  text.textContent = view.cert_pem === null || view.cert_pem === undefined
    ? "" : view.cert_pem;
  box.appendChild(text);

  return box;
}

// httpsSwitch is api.https_enabled. It sits here rather than among the stored
// settings above, because what it decides is everything else in this card: with
// it off there is no certificate to show and no handshake to serve one in.
//
// It saves on its own press. Bound into the form above it would be one more
// value in a save that is mostly about logging, and a flip of it would be lost
// among the rest of what that save reports.
function httpsSwitch(set) {
  const wrap = document.createElement("div");

  wrap.className = "https-switch";

  const row = document.createElement("div");
  row.className = "field";

  const label = element("label", t("certificate.https.label"));
  label.htmlFor = "certificate-https";

  const box = document.createElement("input");
  box.type = "checkbox";
  box.id = "certificate-https";
  box.name = "api_https_enabled";
  box.dataset.field = "api_https_enabled";
  box.checked = Boolean(set.api_https_enabled);

  row.appendChild(label);
  row.appendChild(box);
  row.appendChild(element("small", t("certificate.https.hint")));

  const buttons = document.createElement("div");
  buttons.className = "buttons";
  // It is painted as the main press of this card. A button drawn by the helper
  // is not a submit, and the colour a submit is given comes from a selector that
  // only submits match, so it is asked for by name here.
  buttons.appendChild(actionButton(t("common.save.button"), "certificate-https-save", function () {
    return saveHTTPS(box.checked);
  }, "primary"));

  wrap.appendChild(row);
  wrap.appendChild(buttons);

  return wrap;
}

async function saveHTTPS(enabled) {
  // Only this one setting is sent. The server binds what a request names onto
  // what is stored and leaves the rest alone, so nothing else on the screen is
  // written over by this press.
  await apiCall("PUT", "/api/settings", { api_https_enabled: enabled });

  setNotice(t("certificate.https-saved.notice"), "info");

  return drawSettings();
}

// certificateValidity is the line that says how long is left. A certificate
// that is running out is the one thing on this card that has to be acted on
// before it happens, so it is not left to be worked out from the date above it.
function certificateValidity(view) {
  const days = view.days_remaining;

  if (typeof days !== "number") {
    return statusLine(t("certificate.unknown-days.empty"), "empty");
  }

  if (days < 0) {
    return statusLine(t("certificate.expired.notice"), "warning");
  }

  if (days <= 30) {
    return statusLine(
      t(plural(days, "certificate.expiring-one.notice", "certificate.expiring-many.notice"),
        { days: days }),
      "warning"
    );
  }

  return statusLine(
    t(plural(days, "certificate.good-one.notice", "certificate.good-many.notice"),
      { days: days }),
    "ok"
  );
}

// certificateReplacement is what the last renewal or registration said. The
// sentence about the connections that are already open comes from the server
// and is shown as it came: it is the answer to "I pressed it and the browser
// still shows the old certificate", which is what happens every time.
function certificateReplacement(result) {
  const wrap = document.createElement("div");

  if (typeof result.note === "string" && result.note !== "") {
    wrap.appendChild(statusLine(result.note, "warning"));
  }

  if (typeof result.warning === "string" && result.warning !== "") {
    wrap.appendChild(statusLine(result.warning, "warning"));
  }

  if (typeof result.previous_fingerprint_sha256 === "string" &&
      result.previous_fingerprint_sha256 !== "") {
    const line = element("p", t("certificate.previous.text"));

    line.appendChild(fingerprintValue(result.previous_fingerprint_sha256));
    wrap.appendChild(line);
  }

  return wrap;
}

// fingerprintValue is a fingerprint set apart from the text around it. It is
// read character by character against what a browser or openssl shows, so it
// is not left in the face the rest of the screen is in.
function fingerprintValue(value) {
  const node = element("span", value === null || value === undefined ? "" : value);

  node.className = "fingerprint";

  return node;
}

async function renewCertificate() {
  if (!window.confirm(t("certificate.renew.confirm"))) {
    return;
  }

  certificateReplaceResult = await apiCall("POST", "/api/certificate/renew");
  certificateProblem = "";

  setNotice(t("certificate.renewed.notice"), "info");

  return drawSettings();
}

// certificateForm is where a certificate from somewhere else is registered.
//
// The two are pasted rather than uploaded because what an operator holds is two
// files on the machine they are sitting at, which is not the machine this
// server runs on, and a paste needs nothing on either end but a clipboard.
function certificateForm() {
  const intro = [
    element("p", t("certificate.install-intro.text")),
    element("p", t("certificate.install-chain.text"))
  ];

  // Why the last attempt was refused stays on the form, next to the boxes it is
  // about, and not only on the line above the screen.
  if (certificateProblem !== "") {
    intro.push(statusLine(certificateProblem, "warning"));
  }

  return buildForm({
    name: "certificate-install",
    legend: t("certificate.install.title"),
    submitLabel: t("certificate.install.button"),
    intro: intro,
    fields: [
      {
        name: "cert_pem",
        label: t("certificate.cert.label"),
        type: "textarea",
        value: certificateDraft.certPEM,
        hint: "-----BEGIN CERTIFICATE-----",
        check: checkCertificateBlock,
        note: t("certificate.cert.hint")
      },
      {
        name: "key_pem",
        label: t("form.private-key.label"),
        type: "textarea",
        value: certificateDraft.keyPEM,
        hint: t("form.private-key-example.hint"),
        check: checkKeyBlock,
        note: t("certificate.key.hint")
      }
    ],
    onSubmit: installCertificate
  });
}

// checkCertificateBlock and checkKeyBlock catch the paste that is plainly not
// what the box is for, before a round trip. Everything else, a key that belongs
// to another certificate among it, is for the server to decide: it is the side
// that can actually put the two together.
function checkCertificateBlock(value) {
  return String(value).indexOf("-----BEGIN CERTIFICATE-----") === -1
    ? t("certificate.cert.error")
    : "";
}

function checkKeyBlock(value) {
  const text = String(value);

  if (text.indexOf("-----BEGIN") === -1 || text.indexOf("PRIVATE KEY-----") === -1) {
    return t("form.private-key.error");
  }

  return "";
}

async function installCertificate(values) {
  // What was typed is kept before the call, so that a refusal comes back to a
  // form that still holds it.
  certificateDraft = { certPEM: values.cert_pem, keyPEM: values.key_pem };

  let answer;

  try {
    answer = await apiCall("PUT", "/api/certificate", {
      cert_pem: values.cert_pem,
      key_pem: values.key_pem
    });
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    certificateProblem = error.message;
    setNotice(error.message, "error");

    return drawSettings();
  }

  certificateDraft = { certPEM: "", keyPEM: "" };
  certificateProblem = "";
  certificateReplaceResult = answer;

  setNotice(t("certificate.installed.notice"), "info");

  return drawSettings();
}

// accountCard changes the username, the password or both.
//
// The current password is asked for on every change, the one that only renames
// the account included. A session left open on an unattended screen is
// otherwise all it takes to take the account over, which is the same reason the
// setup runs once and is not a way back here.
function accountCard(account) {
  const name = account.view === null ? "" : account.view.username;
  const intro = [];

  if (name === "") {
    intro.push(statusLine(t("account.unknown.notice", { reason: account.problem }), "warning"));
  } else {
    intro.push(element("p", t("account.name.text", { name: name })));
  }

  intro.push(element("p", t("account.fill.text")));
  intro.push(element("p", t("account.sign-out.text")));

  const form = buildForm({
    name: "account",
    legend: t("account.form.title"),
    submitLabel: t("common.save.button"),
    intro: intro,
    fields: [
      {
        name: "username",
        label: t("account.username.label"),
        note: t("account.username.hint")
      },
      {
        name: "current_password",
        label: t("account.current.label"),
        type: "password",
        check: function (value) {
          return String(value) === "" ? t("account.current.error") : "";
        },
        note: t("account.current.hint")
      },
      {
        name: "new_password",
        label: t("account.password.label"),
        type: "password",
        countBytes: true,
        note: t("account.password.hint")
      },
      passwordConfirmationField("new_password_confirmation", t("setup.password-again.label"),
        "new_password")
    ],
    onSubmit: saveAccount
  });

  form.dataset.card = "settings-account";

  return form;
}

async function saveAccount(values) {
  const username = values.username.trim();
  const newPassword = values.new_password;

  // A request that changes neither is refused by the server too. It is caught
  // here so that the password that was typed in is not sent over the wire to
  // be told that nothing was asked for.
  if (username === "" && newPassword === "") {
    setNotice(t("account.nothing.error"));

    return drawSettings();
  }

  const body = { current_password: values.current_password };

  // A value that is not changing is left out rather than sent as it is. Sending
  // the stored name back would be a change the server refuses, and it is not
  // what was asked for.
  if (username !== "") {
    body.username = username;
  }

  if (newPassword !== "") {
    body.new_password = newPassword;
  }

  const data = await apiCall("PUT", apiAccountPath, body);

  setNotice(accountOutcome(data), "info");

  return drawSettings();
}

// accountOutcome says what the change did. What it says about the other clients
// is half of it: they are signed out wherever they are, and nothing on their
// screens says so until the next thing they press.
function accountOutcome(data) {
  if (data === null || data === undefined) {
    return t("account.changed.notice");
  }

  const said = [];

  if (data.username_changed) {
    said.push(t("account.renamed.notice", { name: data.username }));
  }

  if (data.password_changed) {
    said.push(t("account.password-changed.notice"));
  }

  const ended = data.sessions_ended;

  if (ended === null || ended === undefined || ended === 0) {
    said.push(t("account.none-signed-out.notice"));
  } else {
    said.push(t(plural(ended, "account.signed-out-one.notice", "account.signed-out-many.notice"),
      { count: ended }));
  }

  return said.join(" ");
}

// The four cards below carry this configuration to another installation, and
// take one that arrives from somewhere else. What moves is a file: an export
// hands one out and an import takes one back, so nothing here has to reach the
// other installation and the file is kept wherever the operator keeps it.
//
// transferFileLimit is the largest file the drop areas read. An exported tunnel
// configuration is about half again the size of the JSON it seals, and the JSON
// is a few kilobytes per Host, most of it the private key: 4 MiB is past a
// configuration of several hundred Hosts and small enough that a file dropped
// by mistake is refused before the browser reads it into the page.
const transferFileLimit = 4 * 1024 * 1024;

// revokeObjectURLAfterMs is how long the URL of an exported file is left in
// place before it is handed back. It is not handed back on the next line,
// because a browser that has not started reading the blob by then saves nothing
// at all, and a second is past every browser's start on a file of this size.
const revokeObjectURLAfterMs = 1000;

// transferCard says what the four below are. It is a card of its own so that
// the pair of exports and the pair of imports read as one thing rather than as
// four forms that happen to sit next to each other.
function transferCard() {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "settings-transfer";
  card.appendChild(element("h2", t("transfer.card.title")));
  card.appendChild(element("p", t("transfer.card.text")));
  card.appendChild(element("p", t("transfer.kinds.text")));

  return card;
}

// transferPasswordField is the box the password that seals a file is typed
// into. It is held to what the account is held to, and the count under it is
// there for the same reason it is there under a new account password: the unit
// is bytes, and a password typed in Hangul is three of them per syllable.
function transferPasswordField(note) {
  return {
    name: "password",
    label: t("transfer.password.label"),
    type: "password",
    countBytes: true,
    check: checkPasswordLength,
    note: note
  };
}

// transferFileField is the box an exported file is pasted into, with somewhere
// to drop the file itself beside it. It is the arrangement the private key of a
// Host uses, and for the same reason: the file is on the machine the browser
// runs on and can be dragged in, or it is in a terminal somewhere and gets
// pasted.
function transferFileField(value) {
  return {
    name: "file",
    label: t("transfer.file.label"),
    type: "textarea",
    value: value,
    hint: t("transfer.file-example.hint"),
    check: function (text) {
      return String(text).trim() === "" ? t("transfer.file.error") : "";
    },
    drop: {
      label: t("transfer.drop.label"),
      what: t("transfer.drop-what.text"),
      ever: t("transfer.drop-ever.text"),
      limit: transferFileLimit,
      then: t("transfer.drop-then.text")
    },
    note: t("transfer.file.hint")
  };
}

// transferFilePasswordField is the password of a file that is being imported.
// There is no second box to type it into: the password is not being chosen
// here, and whether it is the right one is something the server answers in a
// sentence that says so.
function transferFilePasswordField() {
  return {
    name: "password",
    label: t("transfer.file-password.label"),
    type: "password",
    check: function (value) {
      return String(value) === "" ? t("transfer.file-password.error") : "";
    },
    note: t("transfer.file-password.hint")
  };
}

function exportTunnelsForm() {
  return buildForm({
    name: "export-tunnels",
    legend: t("transfer.export-tunnels.title"),
    submitLabel: t("transfer.export.button"),
    intro: [
      element("p", t("transfer.export-tunnels.text")),
      statusLine(t("transfer.export-tunnels.notice"), "warning")
    ],
    fields: [
      transferPasswordField(t("transfer.password.hint")),
      passwordConfirmationField("password_confirmation", t("transfer.password-again.label"),
        "password", t("transfer.password-again.hint"))
    ],
    onSubmit: exportTunnels
  });
}

function exportSettingsForm() {
  return buildForm({
    name: "export-settings",
    legend: t("transfer.export-settings.title"),
    submitLabel: t("transfer.export.button"),
    intro: [
      element("p", t("transfer.export-settings.text")),
      element("p", t("transfer.export-settings-sealed.text"))
    ],
    fields: [
      transferPasswordField(t("transfer.password.hint")),
      passwordConfirmationField("password_confirmation", t("transfer.password-again.label"),
        "password", t("transfer.password-again.hint"))
    ],
    onSubmit: exportSettings
  });
}

function importTunnelsForm() {
  const intro = [
    element("p", t("transfer.import-tunnels.text")),
    element("p", t("transfer.import-atomic.text"))
  ];

  // Why the last import was refused stays on the form, next to the boxes it is
  // about, and not only on the line above the screen.
  if (transferProblem.tunnels !== "") {
    intro.push(statusLine(transferProblem.tunnels, "warning"));
  }

  if (transferResult !== null) {
    intro.push(transferItemsTable(transferResult));
  }

  return buildForm({
    name: "import-tunnels",
    legend: t("transfer.import-tunnels.title"),
    submitLabel: t("transfer.import.button"),
    intro: intro,
    fields: [
      transferFileField(transferDraft.tunnels),
      transferFilePasswordField(),
      {
        name: "overwrite",
        label: t("transfer.overwrite.label"),
        type: "checkbox",
        value: false,
        note: t("transfer.overwrite.hint")
      }
    ],
    onSubmit: importTunnels
  });
}

function importSettingsForm() {
  const intro = [
    element("p", t("transfer.import-settings.text")),
    element("p", t("transfer.import-settings-one-row.text"))
  ];

  if (transferProblem.settings !== "") {
    intro.push(statusLine(transferProblem.settings, "warning"));
  }

  return buildForm({
    name: "import-settings",
    legend: t("transfer.import-settings.title"),
    submitLabel: t("transfer.import.button"),
    intro: intro,
    fields: [
      transferFileField(transferDraft.settings),
      transferFilePasswordField()
    ],
    onSubmit: importSettings
  });
}

async function exportTunnels(values) {
  const data = await apiCall("POST", "/api/export/tunnels", { password: values.password });
  const name = handTheFileOut(data, "tunnels");
  const hosts = countOf(data, "hosts");
  const ports = countOf(data, "service_ports");

  setNotice(t(plural(hosts,
    plural(ports, "transfer.exported-host-one-port-one.notice",
      "transfer.exported-host-one-port-many.notice"),
    plural(ports, "transfer.exported-host-many-port-one.notice",
      "transfer.exported-host-many-port-many.notice")),
    { name: name, hosts: hosts, ports: ports }), "info");

  // The screen is drawn again, which is what takes the password out of the box
  // it was typed into. Nothing on the screen repeats it.
  return drawSettings();
}

async function exportSettings(values) {
  const data = await apiCall("POST", "/api/export/settings", { password: values.password });
  const name = handTheFileOut(data, "settings");

  setNotice(t("transfer.exported-settings.notice", { name: name }), "info");

  return drawSettings();
}

// countOf reads one of the counts an export answered with. A count the server
// left out is said as none rather than as the word undefined.
function countOf(data, name) {
  return data === null || data === undefined || typeof data[name] !== "number" ? 0 : data[name];
}

// handTheFileOut gives the sealed file to the browser to save, and says what it
// is called.
//
// The file is built here out of the text the answer carried, rather than the
// link being pointed at the endpoint: the endpoint is a POST with the password
// in the body, so a link to it would have to put that password in a URL, which
// is where it must not be.
function handTheFileOut(data, kind) {
  const text = data === null || data === undefined || typeof data.file !== "string"
    ? "" : data.file;
  const name = transferFileName(kind, data === null || data === undefined
    ? null : data.exported_at);

  const blob = new Blob([text], { type: "application/octet-stream" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");

  link.href = url;
  link.download = name;
  link.click();

  // The URL holds the file in memory for as long as this document lives, and
  // this document lives until the tab is closed: these screens are drawn again
  // in place and never load a page. Handing it back is what keeps a few exports
  // in a row from leaving a copy of each of them behind, and what they are
  // copies of is the credentials of every Host.
  setTimeout(function () {
    URL.revokeObjectURL(url);
  }, revokeObjectURLAfterMs);

  return name;
}

// transferFileName says what the file holds and when it was made, because those
// are the two questions asked of a file found in a directory of downloads a
// year later. The time is the one the server wrote into the file, read in the
// time zone of this browser.
function transferFileName(kind, exportedAt) {
  const said = exportedAt === null || exportedAt === undefined
    ? new Date() : new Date(exportedAt);
  const at = isNaN(said.getTime()) ? new Date() : said;
  const stamp = at.getFullYear() + pad(at.getMonth() + 1) + pad(at.getDate()) + "-" +
    pad(at.getHours()) + pad(at.getMinutes()) + pad(at.getSeconds());

  return "tunnel-manager-" + kind + "-" + stamp + ".tmexport";
}

async function importTunnels(values) {
  // What was pasted is kept before the call, so that a refusal comes back to a
  // form that still holds it.
  transferDraft.tunnels = values.file;

  let answer;

  try {
    answer = await apiCall("POST", "/api/import/tunnels", {
      file: values.file,
      password: values.password,
      overwrite: values.overwrite
    });
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    transferProblem.tunnels = error.message;
    transferResult = null;
    setNotice(error.message, "error");

    return drawSettings();
  }

  transferDraft.tunnels = "";
  transferProblem.tunnels = "";
  transferResult = answer;

  setNotice(importOutcome(answer), "info");

  return drawSettings();
}

// importOutcome is the line above the screen after an import. What became of
// each row is in the table on the card; this is the count, and what to do about
// what was skipped.
function importOutcome(answer) {
  const added = countOf(answer, "added");
  const replaced = countOf(answer, "replaced");
  const skipped = countOf(answer, "skipped");

  if (added + replaced + skipped === 0) {
    return t("transfer.import-empty.notice");
  }

  const counts = { added: added, replaced: replaced, skipped: skipped };

  if (skipped === 0) {
    return t("transfer.imported.notice", counts);
  }

  return t("transfer.imported-skipped.notice", counts);
}

// transferItemsTable is every row of the imported file and what became of it.
// The reason is carried for the rows that were skipped, because that is what
// says whether importing the same file again with the box ticked would change
// anything.
function transferItemsTable(result) {
  const items = result === null || result === undefined ||
    result.items === null || result.items === undefined
    ? [] : result.items;

  const wrap = document.createElement("div");

  wrap.appendChild(element("h3", t("transfer.items.title")));

  if (items.length === 0) {
    wrap.appendChild(statusLine(t("transfer.items-none.empty"), "empty"));

    return wrap;
  }

  wrap.appendChild(buildTable([t("transfer.item-kind.column"), t("transfer.item-name.column"),
    t("transfer.item-action.column"), t("transfer.item-reason.column")],
    items.map(function (item) {
      return [
        transferItemKind(item.kind),
        item.name,
        item.action,
        item.reason === null || item.reason === undefined ? "" : item.reason
      ];
    })));

  return wrap;
}

// transferItemKind names what a row of the file is. A kind this version does
// not know is shown as the server wrote it rather than as an empty cell, so a
// file from a later version is still readable here.
function transferItemKind(kind) {
  if (kind === "host") {
    return t("transfer.kind-host.text");
  }

  if (kind === "service_port") {
    return t("transfer.kind-service-port.text");
  }

  return kind === null || kind === undefined ? "" : String(kind);
}

async function importSettings(values) {
  transferDraft.settings = values.file;

  let answer;

  try {
    answer = await apiCall("POST", "/api/import/settings", {
      file: values.file,
      password: values.password
    });
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    transferProblem.settings = error.message;
    setNotice(error.message, "error");

    return drawSettings();
  }

  transferDraft.settings = "";
  transferProblem.settings = "";

  setNotice(settingsImportOutcome(answer), "info");

  // The screen is drawn again from the server, and that is the point of it:
  // what arrived is stored and is not what this service is running on, so it
  // comes back in the card at the top of this screen, where it is read against
  // what is running and where the restart that puts it into place is pressed.
  return drawSettings();
}

// settingsImportOutcome says what the import stored and, above all, that
// nothing of it is in place yet.
function settingsImportOutcome(answer) {
  const changes = answer === null || answer === undefined ||
    answer.changes === null || answer.changes === undefined
    ? [] : answer.changes;

  if (changes.length === 0) {
    return t("transfer.settings-unchanged.notice");
  }

  return t(plural(changes.length, "transfer.settings-stored-one.notice",
    "transfer.settings-stored-many.notice"), { count: changes.length });
}

// settingsRescue is the way back from a stored setting that keeps the server
// from starting. The form above refuses what the server refuses, so it should
// not happen; it is written down because if it does happen there is no screen
// left to read it on and no configuration file left to correct it in.
function settingsRescue() {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "settings-rescue";
  card.appendChild(element("h2", t("settings.rescue.title")));
  card.appendChild(element("p", t("settings.rescue.text")));
  card.appendChild(element("p", t("settings.rescue-kept.text")));

  return card;
}

// addressAfterRestart is where the service will answer once it has restarted,
// when that is not where this page is talking to it. It is null when the two
// are the same. It reads the same list the card above the form draws, so the
// two cannot disagree.
//
// It matters for more than the wording. This page waits for the service by
// asking the address it is already on, and a service that comes back somewhere
// else never answers there: the wait runs to its limit and reports that
// nothing came back, while the service is up and serving somewhere else.
//
// Two stored settings move it. api.port moves the port, and api.https_enabled
// moves the scheme: a page loaded over http is answered with a redirect to
// https after the restart, and the certificate that redirect leads to is one
// nobody signed for, so the browser refuses it and the wait sees nothing. That
// is the likelier of the two, because turning HTTPS on is a thing an operator
// does from the screen that is served without it.
function addressAfterRestart(set) {
  const pending = set === null || set === undefined ? null : set.pending_restart;
  if (pending === null || pending === undefined) {
    return null;
  }

  let port = null;
  let https = null;

  for (const item of pending) {
    if (item.name === "api.port") {
      port = item.stored;
    }
    if (item.name === "api.https_enabled") {
      https = item.stored === "true";
    }
  }

  if (port === null && https === null) {
    return null;
  }

  // What is not changing is taken from the address this page is on rather than
  // from what is stored, because that is the one this browser actually reached
  // the service at.
  const scheme = https === null
    ? window.location.protocol.replace(":", "")
    : (https ? "https" : "http");
  const here = window.location.port === "" ? "" : window.location.port;
  const where = port === null ? here : port;

  return scheme + "://" + window.location.hostname + (where === "" ? "" : ":" + where) + "/ui/settings";
}

// settingsRestart takes the service down and brings it back. It is a card of
// its own and not part of the danger zone below, which is for what cannot be
// taken back: this one ends with the service running again, and putting it
// among the actions that do not would teach the operator to read past that
// warning.
//
// The password of the account is not asked for here. The uninstall asks because
// what it does is final; a question put to every press is one that stops being
// read.
function settingsRestart(restart, newAddress) {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "settings-restart";
  card.appendChild(element("h2", t("restart.card.title")));
  card.appendChild(element("p", t("restart.card.text")));

  if (restart.view === null) {
    // What a restart does here could not be read, and the two cases it decides
    // between are not the same press at all: after one the service is back by
    // itself, after the other it stays down. Offering the button without
    // knowing which one this is asks the operator to find out by pressing it.
    card.appendChild(statusLine(t("restart.unknown.notice", { reason: restart.problem }),
      "warning"));

    return card;
  }

  card.appendChild(element("p", restartOutcome(restart.view, newAddress)));

  const buttons = document.createElement("div");
  buttons.className = "buttons";

  const button = actionButton(t("settings.restart.button"), "settings-restart", function () {
    return submitRestart(restart.view, button, newAddress);
  });

  button.disabled = restartInFlight;

  buttons.appendChild(button);
  card.appendChild(buttons);

  return card;
}

// restartOutcome is the half of this that differs by platform, in the words the
// card uses. The server says which one it is, because it is the only side that
// knows: the browser cannot tell what the machine on the other end is running.
function restartOutcome(view, newAddress) {
  const address = newAddress === undefined ? null : newAddress;

  if (view.comes_back) {
    if (address === null) {
      return t("restart.in-place-same.text");
    }

    // A stored setting this restart puts into place moves where the service
    // answers. Saying "the same address" here would be wrong in exactly the
    // case an operator is most likely to be restarting for, and the address
    // they have to go to next is the useful part.
    return t("restart.in-place-moved.text", { address: address });
  }

  return t("restart.stops.text");
}

// restartQuestion is what the operator is asked before anything happens. It
// says what is cut either way, and on a platform that does not come back it
// says that too, while there is still something to be done about it.
function restartQuestion(view, newAddress) {
  const address = newAddress === undefined ? null : newAddress;

  if (view.comes_back) {
    if (address === null) {
      return t("restart.ask-same.confirm");
    }

    return t("restart.ask-moved.confirm", { address: address });
  }

  return t("restart.ask-stops.confirm");
}

async function submitRestart(view, button, newAddress) {
  // The press is offered in two places, and a press disables the button it came
  // from and not the other one. Without this, the button that was not pressed
  // asks a server that is already on its way down and puts the failure of that
  // call on the screen of a restart that is going fine.
  if (restartInFlight) {
    return;
  }

  if (!window.confirm(restartQuestion(view, newAddress))) {
    return;
  }

  restartInFlight = true;
  button.disabled = true;

  let answer;

  try {
    answer = await apiCall("POST", "/api/restart");
  } catch (error) {
    // Nothing was taken down, so the card goes back to offering the press.
    restartInFlight = false;
    button.disabled = false;

    throw error;
  }

  if (newAddress !== null) {
    try {
      await moveToTheNewAddress(answer, newAddress);
    } finally {
      restartInFlight = false;
    }

    return;
  }

  drawRestarting(answer, "", newAddress);

  let back = false;

  try {
    back = await waitForTheService(answer);
  } finally {
    restartInFlight = false;
  }

  // The operator may have gone to another screen while this was waiting. What
  // is drawn there is theirs, not this.
  if (currentScreen !== "settings") {
    return;
  }

  if (!back) {
    drawRestarting(answer, t("restart.gave-up.notice", { seconds: restartPollLimitSec }),
      newAddress);

    return;
  }

  setNotice(t("restart.back.notice"), "info");

  return drawSettings();
}

// restartMoveGraceSec is how long the page waits before opening the address the
// service is coming back at.
//
// It cannot be told by asking. The new address is another origin, and the
// certificate behind it is one the browser has not been given a reason to
// trust, so a request to it fails whether the service is up or not and a
// failure says nothing. What is left is to wait out the delay the server named
// for its own exit and then give it room to bind and serve.
//
// Three seconds past the exit is more than it takes: the program replaces its
// own image, so there is no process to start, and the log of a restart has the
// new image reading its settings within a tenth of a second of the old one
// letting go of the port. Opening early costs a page that has to be reloaded,
// which is what the link below it is for.
const restartMoveGraceSec = 3;

// moveToTheNewAddress counts the wait down on screen and then goes there.
//
// The page is left, so nothing after this runs. It is only done for an address
// this page worked out from the settings the restart puts into place, never
// from anything the server sent, so there is no way for an answer to steer the
// browser somewhere of its choosing.
async function moveToTheNewAddress(answer, address) {
  const goingInSec = typeof answer.exit_in_sec === "number" ? answer.exit_in_sec : 0;

  for (let left = goingInSec + restartMoveGraceSec; left > 0; left -= 1) {
    // The operator may have gone to another screen. What is drawn there is
    // theirs, and a page that moved out from under them would be taking the
    // browser somewhere they did not ask to go.
    if (currentScreen !== "settings") {
      return;
    }

    drawRestarting(answer, "", address, left);
    await pause(1000);
  }

  if (currentScreen !== "settings") {
    return;
  }

  drawRestarting(answer, "", address, 0);
  window.location.assign(address);
}

// drawRestarting is the screen while the service is away. It says what was
// asked for, what it does on this platform and how long this page keeps asking,
// so that a wait that is going normally reads as one.
//
// problem is empty while the waiting is still on, and holds what to do about it
// once the page has given up.
function drawRestarting(answer, problem, newAddress, secondsLeft) {
  if (currentScreen !== "settings") {
    return;
  }

  const seconds = typeof answer.exit_in_sec === "number" ? answer.exit_in_sec : 0;
  const nodes = [];

  nodes.push(element("p",
    t(plural(seconds, "restart.asked-one.text", "restart.asked-many.text"),
      { seconds: seconds })));

  nodes.push(element("p", restartOutcome(answer, newAddress)));

  const address = newAddress === undefined ? null : newAddress;

  if (address !== null) {
    // Waiting would be waiting on the wrong address. This page is served from
    // the one being left behind, so asking it again can only ever time out,
    // and reporting that as "it did not come back" would be untrue. It is
    // opened rather than waited on.
    const left = typeof secondsLeft === "number" ? secondsLeft : 0;

    nodes.push(statusLine(
      t(plural(left, "restart.moving-one.empty", "restart.moving-many.empty"),
        { address: address, seconds: left }),
      "empty"));

    // The link is here so that the wait can be skipped, and so that the
    // address survives if the move does not happen: a page that moved on its
    // own and landed on a certificate warning has still said where it went.
    const now = document.createElement("p");
    const link = document.createElement("a");

    link.href = address;
    link.textContent = address;
    now.appendChild(element("span", t("restart.open-now.text")));
    now.appendChild(link);
    nodes.push(now);
  } else if (problem === "") {
    nodes.push(statusLine(
      t(plural(restartPollEverySec, "restart.waiting-one.empty", "restart.waiting-many.empty"),
        { every: restartPollEverySec, limit: restartPollLimitSec }),
      "empty"));
  } else {
    nodes.push(statusLine(problem, "warning"));
    nodes.push(element("p", t("restart.stuck.text")));
  }

  render(t("restart.screen.title"), nodes);
}

// waitForTheService asks until the service answers again or until the limit is
// reached. It reports whether it answered.
async function waitForTheService(answer) {
  const goingInSec = typeof answer.exit_in_sec === "number" ? answer.exit_in_sec : 0;

  // The first ask waits out the delay the server named. Asked before that it is
  // the process that is going down which answers, and a page that took that for
  // the one coming back would say the restart was over before it had begun.
  await pause((goingInSec + 1) * 1000);

  const until = Date.now() + restartPollLimitSec * 1000;

  while (Date.now() < until) {
    const answered = await serviceAnswers();

    if (answered) {
      return true;
    }

    await pause(restartPollEverySec * 1000);
  }

  return false;
}

// serviceAnswers is one ask. Anything that comes back from the server means it
// is serving again, a refusal included: the sessions are held in memory and go
// with the process that held them, so the call that finds the service back is
// usually the one that is told the session has ended. apiCall sends the page to
// the login for that, which is where the operator has to go anyway.
async function serviceAnswers() {
  try {
    await apiCall("GET", "/api/status");

    return true;
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    return false;
  }
}

// pause waits. It is what the loop above is built out of: a timer the browser
// keeps, rather than a spin that holds the one thread this page has.
function pause(milliseconds) {
  return new Promise(function (resolve) {
    window.setTimeout(resolve, milliseconds);
  });
}

// settingsDangerZone is where what cannot be taken back goes. It sits at the
// bottom of the screen and away from Save.
//
// The password of the account is asked for again. It is not the session that is
// in doubt: a screen left open on an unattended desk is one press away from this
// otherwise, and a password is the one thing a passer-by cannot supply.
function settingsDangerZone() {
  const form = buildForm({
    name: "uninstall",
    legend: t("uninstall.form.title"),
    variant: "danger-zone",
    intro: [
      element("p", t("uninstall.intro.text")),
      element("p", t("uninstall.key.text")),
      element("p", t("uninstall.files.text")),
      bulletList([
        t("uninstall.file-database.text"),
        t("uninstall.file-key.text"),
        t("uninstall.file-initial-password.text"),
        t("uninstall.file-logs.text")
      ]),
      element("p", t("uninstall.program.text"))
    ],
    submitLabel: t("uninstall.submit.button"),
    submitVariant: "danger",
    fields: [
      {
        name: "password",
        label: t("uninstall.password.label"),
        type: "password",
        check: function (value) {
          return String(value) === "" ? t("uninstall.password.error") : "";
        },
        note: t("uninstall.password.hint")
      }
    ],
    onSubmit: submitUninstall
  });

  form.dataset.card = "settings-danger";

  return form;
}

async function submitUninstall(values) {
  // The password box is what keeps a passing press from doing this, and the
  // question is what keeps a press that was meant for Save from doing it.
  if (!window.confirm(t("uninstall.ask.confirm"))) {
    return;
  }

  uninstallResult = await apiCall("POST", "/api/uninstall", { password: values.password });

  // From here on nothing asks the server for anything. It removed its own files
  // a moment ago and stops within seconds.
  navigate("uninstalled", { text: t("uninstall.done.notice"), kind: "info" });
}

// drawUninstalled is the screen after the uninstall.
//
// It draws what is already in the browser and makes no call at all, because the
// server it would call is going away as this is drawn. A reload of this path
// does not come back, which is what the screen says: there is nothing left to
// serve the page.
function drawUninstalled() {
  const nodes = [];

  if (uninstallResult === null) {
    // The path was opened without an uninstall having run in this page. Nothing
    // can be looked up, so the screen says only what it knows.
    nodes.push(element("p", t("uninstalled.no-answer.text")));

    render(t("uninstalled.screen.title"), nodes);

    return;
  }

  const seconds = uninstallResult.exit_in_sec;

  nodes.push(element("p", typeof seconds === "number"
    ? t(plural(seconds, "uninstalled.stopped-one.text", "uninstalled.stopped-many.text"),
      { seconds: seconds })
    : t("uninstalled.stopped-soon.text")));

  nodes.push(element("p", t("uninstalled.program.text")));

  const removed = listOfFiles(uninstallResult.removed);

  if (removed.length === 0) {
    nodes.push(statusLine(t("uninstalled.none.empty"), "empty"));
  } else {
    nodes.push(element("h2", t("uninstalled.removed.title")));
    nodes.push(buildTable([t("uninstalled.what.column"), t("uninstalled.path.column")],
      removed.map(function (file) {
        return [file.what, file.path];
      })));
  }

  const failed = listOfFiles(uninstallResult.failed);

  if (failed.length > 0) {
    // These are what is left on disk. The server cannot be asked about them any
    // more, so what it said about each one is shown as it came.
    nodes.push(element("h2", t("uninstalled.left.title")));
    nodes.push(statusLine(t("uninstalled.left.notice"), "warning"));
    nodes.push(buildTable([t("uninstalled.what.column"), t("uninstalled.path.column"),
      t("uninstalled.why.column")], failed.map(function (file) {
        return [file.what, file.path, file.error];
      })));
  }

  render(t("uninstalled.screen.title"), nodes);
}

// listOfFiles is one of the two lists the uninstall answered with. A list the
// server left out arrives as undefined and is read as an empty one.
function listOfFiles(files) {
  return files === null || files === undefined ? [] : files;
}

// drawManual is the manual: what an installation is made of and how a tunnel
// comes to stand, for somebody who has just switched this on.
//
// It asks the server for nothing. Everything on it is true of every
// installation, so there is nothing to fetch and nothing that can be out of
// date, which is also what lets the login show the same thing to a client that
// has no session yet.
function drawManual() {
  render(t("manual.screen.title"), manualNodes());
}

// openManualPanel puts the manual over the screen that asked for it. The login
// is what asks: a panel is laid over #app rather than drawn into it, so the
// form underneath keeps what was typed into it.
function openManualPanel() {
  return openModal({
    name: "manual",
    title: t("manual.screen.title"),
    body: manualNodes(),
    buttons: [{ label: t("common.close.button"), name: "close" }]
  });
}

// manualNodes is what the manual says, as the nodes it is drawn from. The
// screen and the panel both call it, so there is one copy of the words and the
// two cannot drift apart.
//
// The nodes are built again on every call rather than built once and kept. A
// node is in one place at a time, so a list handed to both would leave whichever
// drew last holding it and the other holding nothing.
function manualNodes() {
  return [
    manualWhatItDoes(),
    manualOneTunnel(),
    manualParts(),
    manualReach(),
    manualPeriods(),
    manualPaths()
  ];
}

// manualCard is one section of the manual: a heading, and under it whatever the
// section is made of. A string is a paragraph, which is most of it; the rest are
// the lists and the drawing, which the caller builds.
function manualCard(name, heading, parts) {
  const card = document.createElement("div");

  card.className = "card";
  card.dataset.card = "manual-" + name;
  card.appendChild(element("h2", heading));

  for (const part of parts) {
    card.appendChild(typeof part === "string" ? element("p", part) : part);
  }

  return card;
}

function manualWhatItDoes() {
  return manualCard("what-it-does", t("manual.what-it-does.title"), [
    t("manual.what-it-does-keeps.text"),
    t("manual.what-it-does-nothing.text")
  ]);
}

// manualOneTunnel is the drawing and what is said around it.
//
// It is boxes and not a picture. These screens are read on a phone as often as
// on a desk, and a drawing has one width it was made for: narrowed, it either
// shrinks until the labels cannot be read or takes the page sideways with it.
// Boxes stack instead, which is what the rest of this page does at that width.
function manualOneTunnel() {
  const flow = document.createElement("div");

  flow.className = "flow";
  flow.dataset.flow = "tunnel";

  flow.appendChild(manualFlowStep(1, t("manual.step-manager.title"),
    t("manual.step-manager.text")));
  flow.appendChild(manualFlowArrow());
  flow.appendChild(manualFlowStep(2, t("manual.step-host.title"), t("manual.step-host.text")));
  flow.appendChild(manualFlowArrow());
  flow.appendChild(manualFlowStep(3, t("manual.step-service.title"),
    t("manual.step-service.text")));

  return manualCard("one-tunnel", t("manual.one-tunnel.title"), [
    flow,
    t("manual.one-tunnel-dials.text"),
    t("manual.one-tunnel-columns.text")
  ]);
}

// manualFlowStep is one box of the drawing. The number is in the text of the
// box rather than in a marker beside it, so the order is still readable when
// the boxes stack and the arrows between them turn.
function manualFlowStep(step, title, text) {
  const box = document.createElement("div");

  box.className = "flow-step";
  box.dataset.flowStep = String(step);
  box.appendChild(element("strong", step + ". " + title));
  box.appendChild(element("span", text));

  return box;
}

// manualFlowArrow is what sits between two boxes. It points along the row and
// is turned a quarter turn by the stylesheet where the boxes stack, so it
// points the way the reading goes at either width.
//
// It is kept from screen readers. The numbered boxes carry the order already,
// and read out it is one more character between two paragraphs.
function manualFlowArrow() {
  const arrow = element("span", "\u2192");

  arrow.className = "flow-arrow";
  arrow.setAttribute("aria-hidden", "true");

  return arrow;
}

function manualParts() {
  return manualCard("parts", t("manual.parts.title"), [
    t("manual.parts-three.text"),
    bulletList([
      t("manual.parts-host.text"),
      t("manual.parts-service-port.text"),
      t("manual.parts-assignment.text")
    ]),
    t("manual.parts-built-from.text"),
    t("manual.parts-where.text"),
    t("manual.parts-disabling.text")
  ]);
}

function manualReach() {
  return manualCard("reach", t("manual.reach.title"), [
    t("manual.reach-listener.text"),
    t("manual.reach-column.text"),
    t("manual.reach-unreachable.text")
  ]);
}

function manualPeriods() {
  return manualCard("periods", t("manual.periods.title"), [
    t("manual.periods-two.text"),
    bulletList([
      t("manual.periods-monitoring.text"),
      t("manual.periods-reconcile.text")
    ]),
    t("manual.periods-startup.text"),
    t("manual.periods-answer.text"),
    t("manual.periods-next-start.text")
  ]);
}

function manualPaths() {
  return manualCard("paths", t("manual.paths.title"), [
    t("manual.paths-two.text"),
    bulletList([
      t("manual.paths-absolute.text"),
      t("manual.paths-relative.text")
    ]),
    t("manual.paths-windows.text"),
    t("manual.paths-startup.text")
  ]);
}
