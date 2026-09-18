"use strict";

// The screens of the UI. The key is the path under /ui/, the label is what the
// navigation says, and nav marks the screens that are reached from it: the
// login and the setup are left out because they are what a client without a
// session or without a finished account is sent to, not places to go on a whim.
//
// draw fetches and draws. enter, where a screen has one, is what runs once on
// arrival: it is where a screen resets what it was left in and starts a timer,
// neither of which may happen again on every redraw.
const screens = {
  status: { label: "Status", nav: true, draw: drawStatus, enter: enterStatus },
  hosts: { label: "Hosts", nav: true, draw: drawHosts, enter: enterHosts },
  "service-ports": {
    label: "Service Ports",
    nav: true,
    draw: drawServicePorts,
    enter: enterServicePorts
  },
  logs: { label: "Logs", nav: true, draw: drawLogs, enter: enterLogs },
  settings: { label: "Settings", nav: true, draw: drawSettings, enter: enterSettings },
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

// editingHostID and editingServicePortID say which row has the edit form open.
// Only the identifier is kept: the values in the form come from the last answer
// the list was drawn from, so an edit form never shows a row as it was several
// fetches ago.
let editingHostID = null;
let editingServicePortID = null;

// settingsSaveResult is what the last save on the settings screen answered: the
// settings it changed and whether any of them waits for a restart. It is kept
// because the screen is drawn again from the server right after the save, and
// what the save reported is not in that answer. It is dropped on arrival, so a
// report from an earlier visit is never read as belonging to this one.
let settingsSaveResult = null;

// uninstallResult is what the uninstall answered: the files that went and the
// ones that could not be removed. It is kept here because the screen that shows
// it is drawn after the server has removed itself, so there is nothing left to
// ask for it. A reload finds it empty, and that is what the screen says then.
let uninstallResult = null;

// logOut ends the session and goes to the login. The cookie is dropped by the
// server, so nothing here has to be cleared.
async function logOut() {
  await apiCall("POST", "/api/logout");

  navigate("login", { text: "You are signed out.", kind: "info" });
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
    legend: "Sign in",
    submitLabel: "Sign in",
    fields: [
      {
        name: "username",
        label: "Username",
        note: "Leave this empty on the first sign in, before the account is set up."
      },
      { name: "password", label: "Password", type: "password" }
    ],
    onSubmit: submitLogin
  });

  render("Tunnel Manager", [form]);
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
      text: "Choose a username and a password for this installation.",
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
    legend: "Set up the account",
    submitLabel: "Save",
    fields: [
      { name: "username", label: "Username" },
      {
        name: "password",
        label: "New password",
        type: "password",
        countBytes: true,
        note: "The length is counted in bytes. One Hangul syllable counts as three."
      }
    ],
    onSubmit: submitSetup
  });

  render("Tunnel Manager", [form]);
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

  navigate("status", { text: "The account is set up.", kind: "info" });
}

// enterStatus draws the screen and starts the refresh. The period is the one
// the reconcile loop runs at, so a tunnel that comes up shows up within a pass
// of it. The timer is stopped by showScreen when the screen is left.
function enterStatus() {
  const drawn = drawStatus();

  refreshTimer = window.setInterval(function () {
    run(drawStatus);
  }, statusRefreshMs);

  return drawn;
}

async function drawStatus() {
  const data = await apiCall("GET", "/api/status");

  // A refresh that was in flight while the operator left must not draw over
  // the screen they went to.
  if (currentScreen !== "status") {
    return;
  }

  const counts = document.createElement("div");
  counts.className = "counts";
  counts.appendChild(countBox("Desired", data.desired_tunnels, "desired"));
  counts.appendChild(countBox("Rows", data.total_tunnels, "total"));
  counts.appendChild(countBox("Connected", data.connected_tunnels, "connected"));

  const nodes = [counts];

  // The three counts differ for two different reasons, and the difference is
  // the whole point of showing all three. A tunnel with no row has not been
  // started at all, while a row that is not connected was started and failed.
  const missing = data.desired_tunnels - data.total_tunnels;
  if (missing > 0) {
    nodes.push(statusLine(
      missing + " " + plural(missing, "tunnel that should", "tunnels that should") +
        " be running " + plural(missing, "has", "have") + " no row yet.",
      "warning"
    ));
  }

  const down = data.total_tunnels - data.connected_tunnels;
  if (down > 0) {
    nodes.push(statusLine(
      down + " " + plural(down, "tunnel has a row but is", "tunnels have rows but are") +
        " not connected.",
      "warning"
    ));
  }

  if (missing === 0 && down === 0) {
    nodes.push(statusLine("Every tunnel that should be running is connected.", "ok"));
  }

  const tunnels = data.tunnels === null || data.tunnels === undefined ? [] : data.tunnels;

  if (tunnels.length === 0) {
    nodes.push(statusLine("There are no tunnels.", "empty"));
  } else {
    const rows = tunnels.map(function (tunnel) {
      return [
        tunnel.host_id,
        tunnel.sp_id,
        statusBadge(tunnel.status),
        tunnel.server,
        tunnel.local,
        tunnel.remote,
        tunnel.retry_count,
        timeCell(tunnel.last_connected_at),
        tunnel.last_error
      ];
    });

    nodes.push(buildTable(
      ["Host", "Service port", "Status", "Server", "Local", "Remote", "Retries",
        "Last connected", "Last error"],
      rows,
      [0, 1, 6]
    ));
  }

  render("Status", nodes);
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

function enterHosts() {
  editingHostID = null;

  return drawHosts();
}

async function drawHosts() {
  const answer = await apiCall("GET", "/api/host");
  const hosts = answer === null ? [] : answer;

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
    nodes.push(statusLine("There are no hosts.", "empty"));
  } else {
    nodes.push(buildTable(
      ["ID", "IP", "Port", "User", "Description", "Enabled", "Updated", ""],
      hosts.map(hostRow),
      [0, 2]
    ));
  }

  render("Hosts", nodes);
}

function hostRow(host) {
  const buttons = document.createElement("div");

  buttons.className = "buttons";
  buttons.appendChild(actionButton("Edit", "host-edit-" + host.id, function () {
    editingHostID = host.id;

    return drawHosts();
  }));
  buttons.appendChild(actionButton(
    host.enabled ? "Disable" : "Enable",
    "host-toggle-" + host.id,
    function () {
      return toggleHost(host);
    }
  ));
  buttons.appendChild(actionButton("Delete", "host-delete-" + host.id, function () {
    return deleteHost(host);
  }, "danger"));

  return [
    host.id,
    host.ip,
    host.port,
    host.user,
    host.description,
    host.enabled ? "yes" : "no",
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
    hint: "192.0.2.10 or 2001:db8::1",
    filter: ipCharacters,
    check: checkIP
  };
}

function portField(name, label, value) {
  return {
    name: name,
    label: label,
    value: value,
    hint: "1 to 65535",
    inputMode: "numeric",
    filter: portCharacters,
    check: checkPort
  };
}

function hostCreateForm() {
  return buildForm({
    name: "host-create",
    legend: "Add a host",
    submitLabel: "Add",
    fields: [
      ipField("ip", "IP"),
      portField("port", "SSH port", 22),
      { name: "user", label: "User" },
      { name: "password", label: "Password", type: "password" },
      { name: "description", label: "Description" }
    ],
    onSubmit: createHost
  });
}

function hostEditForm(host) {
  return buildForm({
    name: "host-edit",
    legend: "Edit host " + host.id,
    submitLabel: "Save",
    fields: [
      ipField("ip", "IP", host.ip),
      portField("port", "SSH port", host.port),
      { name: "user", label: "User", value: host.user },
      {
        name: "password",
        label: "Password",
        type: "password",
        note: "Leave this empty to keep the password that is stored."
      },
      { name: "description", label: "Description", value: host.description },
      { name: "enabled", label: "Enabled", type: "checkbox", value: host.enabled }
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
    description: values.description
  };

  body.password = values.password;

  await apiCall("POST", "/api/host", body);

  setNotice("Host " + body.ip + " was added.", "info");

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

  await apiCall("PUT", "/api/host/" + host.id, body);

  editingHostID = null;
  setNotice("Host " + host.id + " was updated.", "info");

  return drawHosts();
}

async function toggleHost(host) {
  await apiCall("PUT", "/api/host/" + host.id, { enabled: !host.enabled });

  setNotice("Host " + host.id + " is now " + (host.enabled ? "disabled" : "enabled") + ".", "info");

  return drawHosts();
}

async function deleteHost(host) {
  // Deleting a host takes its tunnels down with it, which is not something the
  // operator can take back with another click.
  if (!window.confirm("Delete host " + host.id + " (" + host.ip + ")?")) {
    return;
  }

  await apiCall("DELETE", "/api/host/" + host.id);

  if (editingHostID === host.id) {
    editingHostID = null;
  }

  setNotice("Host " + host.id + " was deleted.", "info");

  return drawHosts();
}

function enterServicePorts() {
  editingServicePortID = null;

  return drawServicePorts();
}

async function drawServicePorts() {
  const answer = await apiCall("GET", "/api/service-port");
  const ports = answer === null ? [] : answer;

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
    nodes.push(statusLine("There are no service ports.", "empty"));
  } else {
    nodes.push(buildTable(
      ["ID", "Service IP", "Service port", "Local port", "Description", "Updated", ""],
      ports.map(servicePortRow),
      [0, 2, 3]
    ));
  }

  render("Service Ports", nodes);
}

function servicePortRow(port) {
  const buttons = document.createElement("div");

  buttons.className = "buttons";
  buttons.appendChild(actionButton("Edit", "service-port-edit-" + port.id, function () {
    editingServicePortID = port.id;

    return drawServicePorts();
  }));
  buttons.appendChild(actionButton("Delete", "service-port-delete-" + port.id, function () {
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
    legend: "Add a service port",
    submitLabel: "Add",
    fields: [
      ipField("service_ip", "Service IP"),
      portField("service_port", "Service port"),
      portField("local_port", "Local port"),
      { name: "description", label: "Description" }
    ],
    onSubmit: createServicePort
  });
}

function servicePortEditForm(port) {
  return buildForm({
    name: "service-port-edit",
    legend: "Edit service port " + port.id,
    submitLabel: "Save",
    fields: [
      ipField("service_ip", "Service IP", port.service_ip),
      portField("service_port", "Service port", port.service_port),
      portField("local_port", "Local port", port.local_port),
      { name: "description", label: "Description", value: port.description }
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

  await apiCall("POST", "/api/service-port", body);

  setNotice("Service port " + body.service_ip + ":" + body.service_port + " was added.", "info");

  return drawServicePorts();
}

async function updateServicePort(port, values) {
  await apiCall("PUT", "/api/service-port/" + port.id, servicePortBody(values));

  editingServicePortID = null;
  setNotice("Service port " + port.id + " was updated.", "info");

  return drawServicePorts();
}

async function deleteServicePort(port) {
  // The tunnels that carry this service port go down with it.
  if (!window.confirm("Delete service port " + port.id + " (" + port.service_ip + ":" +
      port.service_port + ")?")) {
    return;
  }

  await apiCall("DELETE", "/api/service-port/" + port.id);

  if (editingServicePortID === port.id) {
    editingServicePortID = null;
  }

  setNotice("Service port " + port.id + " was deleted.", "info");

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

    run(drawLogs);
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

    render("Logs", nodes);

    return;
  }

  const lines = answer === null || answer.lines === null || answer.lines === undefined
    ? []
    : answer.lines;

  const shown = lines.filter(keepLogLine);

  nodes.push(logSummary(answer, lines.length, shown.length));

  if (answer.capped) {
    nodes.push(statusLine(
      "The read reached its byte limit before " + logLineCount + " lines were found, so " +
        "the oldest line below is not as far back as was asked for.",
      "warning"
    ));
  }

  if (shown.length === 0) {
    nodes.push(statusLine(
      lines.length === 0
        ? "The log file holds nothing yet."
        : "None of the " + lines.length + " " + plural(lines.length, "line", "lines") +
          " read is at " + logLevelFilter + " or above.",
      "empty"
    ));
  } else {
    // The server hands the lines over in the order they are in the file, oldest
    // first, and they are turned around here. The newest line is what the
    // screen is opened for, and at the top it is in the same place after every
    // refresh instead of moving down as the log grows.
    nodes.push(buildTable(["Time", "Level", "Caller", "Message"], shown.reverse().map(logRow)));
  }

  render("Logs", nodes);
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
function logTimeCell(value) {
  const node = element("span", value === null || value === undefined ? "" : value);

  node.className = "stamp";

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
function logCallerCell(value) {
  const node = element("span", value === null || value === undefined ? "" : value);

  node.className = "log-caller";

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

  row.appendChild(logSelect("log-lines", "Lines", logLineCounts, logLineCount,
    function (value) {
      logLineCount = value;

      return drawLogs();
    }));

  row.appendChild(logSelect("log-level", "Level at least", [logLevelAll].concat(logLevels),
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
  auto.appendChild(element("span", "Refresh every " + statusRefreshMs / 1000 + " seconds"));
  row.appendChild(auto);

  row.appendChild(actionButton("Refresh now", "log-refresh", drawLogs));

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
  const note = element("p",
    "Only the file that is being written to now is read. The rotated files beside it are " +
      "not shown, and neither is the console, which is where the server writes when the " +
      "log file cannot be opened. The newest line is at the top.");

  note.className = "log-scope";

  return note;
}

// logSummary says what was read to draw the table.
//
// The two sizes are in it because they are what shows that the whole file is
// not being pulled across: the log is allowed to reach a hundred megabytes
// before it rotates, and what was read to fill this screen is the end of it.
function logSummary(answer, read, shown) {
  const counted = shown === read
    ? "Showing " + shown + " " + plural(shown, "line", "lines")
    : "Showing " + shown + " of the " + read + " " + plural(read, "line", "lines") + " read";

  return statusLine(counted + ", from the last " + formatBytes(answer.read) + " of the " +
    formatBytes(answer.size) + " in " + answer.path + ".", "empty");
}

function enterSettings() {
  settingsSaveResult = null;

  return drawSettings();
}

async function drawSettings() {
  const set = await apiCall("GET", "/api/settings");

  const nodes = [];

  // What the save did sits above the form. It is the answer to the press that
  // was just made, and the form below it is filled from the server anyway.
  if (settingsSaveResult !== null) {
    nodes.push(settingsChanges(settingsSaveResult));
  }

  nodes.push(settingsForm(set));
  nodes.push(settingsRescue());
  nodes.push(settingsDangerZone());

  render("Settings", nodes);
}

// settingsForm is every setting that is stored. The boxes are checked here
// against the rules the server holds, so a value it would refuse is reported
// next to the box it was typed in rather than after a round trip. A value that
// gets past this is still checked by the server: this form is a convenience,
// not the rule.
function settingsForm(set) {
  return buildForm({
    name: "settings",
    legend: "Stored settings",
    submitLabel: "Save",
    fields: [
      settingsField(portField("api_port", "API port", set.api_port),
        "The server listens on this port. It is taken up at the next start."),
      settingsField(secondsField("monitoring_interval_sec", "Monitoring interval (seconds)",
        set.monitoring_interval_sec), "Taken up at the next start."),
      settingsField(secondsField("reconcile_interval_sec", "Reconcile interval (seconds)",
        set.reconcile_interval_sec), "Taken up at the next start."),
      {
        name: "security_key_file",
        label: "Encryption key file",
        value: set.security_key_file,
        check: checkPath,
        note: "The path of the file the key is kept in. The key itself is never " +
          "shown here. Taken up at the next start."
      },
      {
        name: "logging_level",
        label: "Log level",
        value: set.logging_level,
        options: logLevels,
        note: "This one takes hold the moment it is saved."
      },
      {
        name: "logging_format",
        label: "Log format",
        value: set.logging_format,
        options: logFormats,
        note: "Taken up at the next start."
      },
      settingsField(
        { name: "logging_file_path", label: "Log file", value: set.logging_file_path,
          check: checkPath },
        "Taken up at the next start."
      ),
      settingsField(countField("logging_file_max_size", "Log size before rotation (MB)",
        set.logging_file_max_size), "Taken up at the next start."),
      settingsField(countField("logging_file_max_backups", "Rotated files kept",
        set.logging_file_max_backups), "Taken up at the next start."),
      settingsField(countField("logging_file_max_age", "Days a rotated file is kept",
        set.logging_file_max_age), "Taken up at the next start."),
      {
        name: "logging_file_compress",
        label: "Compress rotated files",
        type: "checkbox",
        value: set.logging_file_compress,
        note: "Taken up at the next start."
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
    hint: "seconds",
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
    hint: "0 or more",
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

  settingsSaveResult = data;

  const changes = data === null || data.changes === null || data.changes === undefined
    ? []
    : data.changes;

  if (changes.length === 0) {
    setNotice("The settings are stored. Nothing changed.", "info");
  } else if (data.restart_required) {
    setNotice("The settings are stored. Some of them are taken up at the next start.", "info");
  } else {
    setNotice("The settings are stored and are in place.", "info");
  }

  return drawSettings();
}

// settingsChanges is what the save did. What changed is worth showing on its
// own, because a save that stores what was already there and a save that
// changed the port look the same on the form afterwards.
//
// Whether a change is in place or waits for a start is what the server said
// about it. It is the side that puts a value into place, so a screen that
// decided for itself would go on claiming a setting took hold after the server
// stopped putting it there.
function settingsChanges(result) {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "settings-saved";
  card.appendChild(element("h2", "What the save changed"));

  const changes = result === null || result.changes === null || result.changes === undefined
    ? []
    : result.changes;

  if (changes.length === 0) {
    card.appendChild(statusLine("Nothing changed.", "empty"));

    return card;
  }

  card.appendChild(buildTable(
    ["Setting", "From", "To", "Applied"],
    changes.map(function (change) {
      return [change.name, change.from, change.to, appliedBadge(change.applied)];
    })
  ));

  if (result.restart_required) {
    card.appendChild(statusLine(
      "Start tunnel-manager again to run on what is marked as taken up at the next start. " +
        "It is stored either way.",
      "warning"
    ));
  } else {
    card.appendChild(statusLine("Every change is in place.", "ok"));
  }

  return card;
}

// appliedBadge says what became of one change. A word that is not one of the
// two known ones is shown as it came, so a server that reports a third state
// still says something readable here.
function appliedBadge(applied) {
  const words = { now: "in place now", restart: "at the next start" };
  const colours = { now: "ok", restart: "waiting" };
  const text = applied === null || applied === undefined ? "" : String(applied);
  const badge = element("span", words[text] === undefined ? text : words[text]);

  badge.className = "badge " + (colours[text] === undefined ? "unknown" : colours[text]);
  badge.dataset.applied = text;

  return badge;
}

// settingsRescue is the way back from a stored setting that keeps the server
// from starting. The form above refuses what the server refuses, so it should
// not happen; it is written down because if it does happen there is no screen
// left to read it on and no configuration file left to correct it in.
function settingsRescue() {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "settings-rescue";
  card.appendChild(element("h2", "If the server will not start on what is stored"));
  card.appendChild(element("p",
    "Start it once with -reset-settings. Every setting goes back to its default, " +
      "what it changed is printed, and the process exits. The next start runs on " +
      "the defaults, and this screen is reachable again."));

  return card;
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
    legend: "Dangerous actions",
    variant: "danger-zone",
    intro: [
      element("p",
        "Uninstall stops every tunnel, removes the files this installation is made of " +
          "and ends the process. Nothing here can be taken back."),
      element("p",
        "Removing the encryption key is the part nothing undoes. The SSH password of " +
          "every host is sealed with that key, so a backup of the database taken " +
          "beforehand cannot be read once the key is gone: the passwords in it stay " +
          "unreadable and have to be typed in again on a fresh installation."),
      element("p", "These files are removed:"),
      bulletList([
        "The database file, along with the -wal and -shm files SQLite keeps beside it",
        "The encryption key file",
        "The initial password file, if it is still there",
        "The log file and the rotated log files beside it"
      ]),
      element("p",
        "The program file is left where it is. A running process cannot remove its own " +
          "image on every system this runs on, so removing it is left to you once the " +
          "process has stopped.")
    ],
    submitLabel: "Uninstall",
    submitVariant: "danger",
    fields: [
      {
        name: "password",
        label: "Password",
        type: "password",
        check: function (value) {
          return String(value) === "" ? "Enter the password of this account." : "";
        },
        note: "The password this account is signed in with. It is asked for again so " +
          "that a screen left open cannot be uninstalled with one press."
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
  if (!window.confirm("Uninstall tunnel-manager? The database, the encryption key and " +
      "the logs are removed and the process stops.")) {
    return;
  }

  uninstallResult = await apiCall("POST", "/api/uninstall", { password: values.password });

  // From here on nothing asks the server for anything. It removed its own files
  // a moment ago and stops within seconds.
  navigate("uninstalled", { text: "The installation was removed.", kind: "info" });
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
    nodes.push(element("p",
      "This screen is drawn from what the uninstall answered, and this page holds no " +
        "answer. If the uninstall ran, the server is gone and there is nothing left to ask."));

    render("Uninstalled", nodes);

    return;
  }

  const seconds = uninstallResult.exit_in_sec;

  nodes.push(element("p",
    "Every tunnel was stopped, the database was closed and the files below were removed. " +
      "The process stops " +
      (typeof seconds === "number" ? "about " + seconds + " " + plural(seconds, "second", "seconds") +
        " after this screen appeared" : "a few seconds after this screen appeared") +
      ". Reloading this page will not bring it back."));

  nodes.push(element("p",
    "The program file is still where it was. Remove it by hand, along with the service " +
      "entry that starts it, if this installation was set up as a service."));

  const removed = listOfFiles(uninstallResult.removed);

  if (removed.length === 0) {
    nodes.push(statusLine("No file was there to remove.", "empty"));
  } else {
    nodes.push(element("h2", "Removed"));
    nodes.push(buildTable(["What", "Path"], removed.map(function (file) {
      return [file.what, file.path];
    })));
  }

  const failed = listOfFiles(uninstallResult.failed);

  if (failed.length > 0) {
    // These are what is left on disk. The server cannot be asked about them any
    // more, so what it said about each one is shown as it came.
    nodes.push(element("h2", "Left behind"));
    nodes.push(statusLine(
      "These could not be removed and are still on disk. Remove them by hand.",
      "warning"
    ));
    nodes.push(buildTable(["What", "Path", "Why it stayed"], failed.map(function (file) {
      return [file.what, file.path, file.error];
    })));
  }

  render("Uninstalled", nodes);
}

// listOfFiles is one of the two lists the uninstall answered with. A list the
// server left out arrives as undefined and is read as an empty one.
function listOfFiles(files) {
  return files === null || files === undefined ? [] : files;
}
