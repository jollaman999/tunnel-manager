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
      },
      passwordConfirmationField("password_confirmation", "New password again", "password")
    ],
    onSubmit: submitSetup
  });

  render("Tunnel Manager", [form]);
}

// passwordConfirmationField is the second box a new password is typed into. It
// is checked against the box named by against, and nothing is sent until the
// two hold the same thing.
//
// What is sent is the password itself, once. The server is never handed the
// second copy: it would have nothing to learn from the same string twice, and
// the typo this box is here for is made in this browser.
function passwordConfirmationField(name, label, against) {
  return {
    name: name,
    label: label,
    type: "password",
    check: function (value, values) {
      return checkPasswordConfirmation(value, values[against]);
    },
    note: "Type the new password a second time. A slip at the keyboard is caught here " +
      "rather than at the next sign in."
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
      const cells = [
        tunnel.host_id,
        tunnel.sp_id,
        statusBadge(tunnel.status),
        tunnel.server,
        tunnel.local,
        tunnel.remote,
        tunnel.retry_count,
        timeCell(tunnel.last_connected_at)
      ];

      // The last error goes under the row rather than in it. Eight columns of
      // addresses and counts already ask for more width than a screen has, and
      // what is left for a column holding a sentence was measured at 144px
      // against a row that stood 183px tall. Under the row it has the width of
      // the table, and a tunnel with nothing wrong carries no line at all.
      const failure = typeof tunnel.last_error === "string" ? tunnel.last_error : "";
      if (failure === "") {
        return cells;
      }

      const said = element("span", failure);

      said.className = "last-error";

      return { cells: cells, under: said };
    });

    nodes.push(buildTable(
      ["Host", "Service port", "Status", "Server", "Local", "Remote", "Retries",
        "Last connected"],
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
    label: "Private key (PEM)",
    type: "textarea",
    hint: "The key file, which begins with a BEGIN PRIVATE KEY line",
    check: checkPrivateKeyBlock,
    drop: { label: "Drop the key file here, or paste it into the box above." },
    note: note
  };
}

function keyPassphraseField() {
  return {
    name: "key_passphrase",
    label: "Key passphrase",
    type: "password",
    note: "Only if the key is protected by one. It is stored encrypted, the same as the key."
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
    return "Paste the private key. Its first line is the one that says BEGIN PRIVATE KEY.";
  }

  return "";
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
      privateKeyField("Stored encrypted. It is never shown on this screen and never sent back."),
      keyPassphraseField(),
      {
        name: "password",
        label: "Password",
        type: "password",
        note: "Leave this empty if you registered a key. A host that carries both is tried " +
          "with the key first and falls back to the password."
      },
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
      privateKeyField("Leave this empty to keep the key that is stored. A key that is stored is " +
        "never shown here. A key that is sent replaces the stored key and its passphrase together."),
      keyPassphraseField(),
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

  // The key boxes are sent only when they hold something, so that a host
  // registered with a password alone carries no empty key.
  const privateKey = values.private_key.trim();
  if (privateKey !== "") {
    body.private_key = privateKey;
    body.key_passphrase = values.key_passphrase;
  }

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
    const table = buildTable(["Time", "Level", "Caller", "Message"], shown.reverse().map(logRow));

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
  certificateDraft = { certPEM: "", keyPEM: "" };
  certificateProblem = "";
  certificateReplaceResult = null;

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
  const pending = settingsPending(set);
  if (pending !== null) {
    nodes.push(pending);
  }

  nodes.push(settingsForm(set));
  nodes.push(certificateCard(set, certificate));
  nodes.push(certificateForm());
  nodes.push(accountCard(account));
  nodes.push(settingsRescue());
  nodes.push(settingsRestart(restart, addressAfterRestart(set)));
  nodes.push(settingsDangerZone());

  render("Settings", nodes);
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

  const base = whatItIs + " Taken up at the next start.";

  if (dir === "") {
    return base;
  }

  // A drive letter and a colon is what Windows calls the start of an absolute
  // path, and it is also how this screen can tell which platform it is looking
  // at without being told.
  const windows = /^[A-Za-z]:[\\/]/.test(dir);

  if (windows) {
    return base + " An absolute path is used as it stands, and on Windows that means one " +
      "naming a drive or a share, as in " + dir + "\\logs\\tunnel-manager.log. Anything " +
      "else is read against " + dir + ", the directory the database file is in, and that " +
      "includes a path beginning with a single backslash, which Windows does not count as " +
      "absolute.";
  }

  return base + " A path beginning with / is used as it stands. Anything else is read " +
    "against " + dir + ", the directory the database file is in.";
}

function settingsForm(set) {
  return buildForm({
    name: "settings",
    legend: "Stored settings",
    submitLabel: "Save",
    fields: [
      settingsField(portField("api_port", "API port", set.api_port),
        "The server listens on this port. It is taken up at the next start."),
      settingsField(secondsField("monitoring_interval_sec", "Monitoring interval (seconds)",
        set.monitoring_interval_sec),
      "How often a tunnel that is up checks that the SSH server is still answering, and " +
        "reconnects when it is not. Shorter notices a connection that died sooner and reaches " +
        "the server more often. Taken up at the next start."),
      settingsField(secondsField("reconcile_interval_sec", "Reconcile interval (seconds)",
        set.reconcile_interval_sec),
      "How often the tunnels that are running are compared with the Hosts and service ports " +
        "that are registered. A tunnel that should exist is started, one that should not is " +
        "stopped, and one whose settings changed is built again. Taken up at the next start."),
      {
        name: "security_key_file",
        label: "Encryption key file",
        value: set.security_key_file,
        check: checkPath,
        note: pathNote(set, "The path of the file the key is kept in. The key itself is " +
          "never shown here.")
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
        pathNote(set, "The path of the file the log is written to.")
      ),
      settingsField(countField("logging_file_max_size", "Log size before it is rotated (MB)",
        set.logging_file_max_size), "Taken up at the next start."),
      settingsField(countField("logging_file_max_backups", "Rotated log files kept",
        set.logging_file_max_backups), "Taken up at the next start."),
      settingsField(countField("logging_file_max_age", "Days a rotated log file is kept",
        set.logging_file_max_age), "Taken up at the next start."),
      {
        name: "logging_file_compress",
        label: "Compress rotated log files",
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
function settingsPending(set) {
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
  card.appendChild(element("h2", "Stored, waiting for a restart"));
  card.appendChild(element("p",
    "These settings are stored with a value this service is not running on. It goes on " +
      "running on what it read when it started until it is started again."));
  card.appendChild(buildTable(
    ["Setting", "Running on", "Stored"],
    pending.map(function (item) {
      return [item.name, item.running, item.stored];
    })
  ));
  card.appendChild(statusLine(
    "Restart the service, further down this screen, is what puts them into place.",
    "warning"
  ));

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
  card.appendChild(element("h2", "HTTPS and the certificate"));
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

  card.appendChild(buildTable(["What", "Value"], [
    ["Fingerprint (SHA-256)", fingerprintValue(view.fingerprint_sha256)],
    ["Subject", view.subject],
    ["Issuer", view.issuer],
    ["Signed by", view.self_signed
      ? "Itself. A client warns about it until this certificate is trusted on that machine"
      : "Another certificate, which the issuer above names"],
    ["Names and addresses it covers", hosts.length === 0 ? "None" : hosts.join(", ")],
    ["Valid from", formatTime(view.not_before)],
    ["Valid until", formatTime(view.not_after)]
  ]));

  card.appendChild(certificateValidity(view));
  card.appendChild(certificatePEM(view));

  const buttons = document.createElement("div");
  buttons.className = "buttons";
  buttons.appendChild(actionButton("Make a new certificate", "certificate-renew",
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
  label.textContent = "The certificate as PEM";
  box.appendChild(label);

  const note = document.createElement("p");
  note.className = "note";
  note.textContent = "Save this as a file and add it to the trust store of the " +
    "machine you browse from, or pass it to curl with --cacert. Nothing in it is " +
    "private: the server hands these same bytes to every client.";
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

  const label = element("label", "Serve over HTTPS");
  label.htmlFor = "certificate-https";

  const box = document.createElement("input");
  box.type = "checkbox";
  box.id = "certificate-https";
  box.name = "api_https_enabled";
  box.dataset.field = "api_https_enabled";
  box.checked = Boolean(set.api_https_enabled);

  row.appendChild(label);
  row.appendChild(box);
  row.appendChild(element("small",
    "Taken up at the next start. With it off the API and these screens are served in the " +
      "clear, and everything they send travels as it is, the password of this account among it."));

  const buttons = document.createElement("div");
  buttons.className = "buttons";
  // It is painted as the main press of this card. A button drawn by the helper
  // is not a submit, and the colour a submit is given comes from a selector that
  // only submits match, so it is asked for by name here.
  buttons.appendChild(actionButton("Save", "certificate-https-save", function () {
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

  setNotice("The setting is stored. It is taken up at the next start.", "info");

  return drawSettings();
}

// certificateValidity is the line that says how long is left. A certificate
// that is running out is the one thing on this card that has to be acted on
// before it happens, so it is not left to be worked out from the date above it.
function certificateValidity(view) {
  const days = view.days_remaining;

  if (typeof days !== "number") {
    return statusLine("How long this certificate has left could not be read.", "empty");
  }

  if (days < 0) {
    return statusLine(
      "This certificate has expired. Nothing connects to this server over HTTPS until it is " +
        "replaced: make a new one below, or register one you were given.",
      "warning"
    );
  }

  if (days <= 30) {
    return statusLine(
      "This certificate runs out in " + days + " " + plural(days, "day", "days") +
        ". Replace it before then, or nothing will connect over HTTPS.",
      "warning"
    );
  }

  return statusLine(
    "This certificate is good for another " + days + " " + plural(days, "day", "days") + ".",
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
    const line = element("p", "The fingerprint before this change was ");

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
  if (!window.confirm("Make a new certificate? Its fingerprint is a different one, so every " +
      "browser and every script that was told to trust the current certificate warns about " +
      "this server again until the new one is trusted as well. Connections that are open now " +
      "are not cut.")) {
    return;
  }

  certificateReplaceResult = await apiCall("POST", "/api/certificate/renew");
  certificateProblem = "";

  setNotice("A new certificate is in place for every connection made from now on. Reload the " +
    "page to be served it.", "info");

  return drawSettings();
}

// certificateForm is where a certificate from somewhere else is registered.
//
// The two are pasted rather than uploaded because what an operator holds is two
// files on the machine they are sitting at, which is not the machine this
// server runs on, and a paste needs nothing on either end but a clipboard.
function certificateForm() {
  const intro = [
    element("p",
      "Paste a certificate you were issued, together with its private key. Both are stored " +
        "in the database, the key encrypted with the same key the SSH passwords are sealed " +
        "with, and the certificate is served from the next connection on."),
    element("p",
      "If the issuer gave you intermediate certificates, paste them into the same box, below " +
        "the server certificate and in the order they were given. The server certificate goes " +
        "first.")
  ];

  // Why the last attempt was refused stays on the form, next to the boxes it is
  // about, and not only on the line above the screen.
  if (certificateProblem !== "") {
    intro.push(statusLine(certificateProblem, "warning"));
  }

  return buildForm({
    name: "certificate-install",
    legend: "Register a certificate of your own",
    submitLabel: "Register certificate",
    intro: intro,
    fields: [
      {
        name: "cert_pem",
        label: "Certificate (PEM)",
        type: "textarea",
        value: certificateDraft.certPEM,
        hint: "-----BEGIN CERTIFICATE-----",
        check: checkCertificateBlock,
        note: "The server certificate first, then any intermediates."
      },
      {
        name: "key_pem",
        label: "Private key (PEM)",
        type: "textarea",
        value: certificateDraft.keyPEM,
        hint: "The key file, which begins with a BEGIN PRIVATE KEY line",
        check: checkKeyBlock,
        note: "Stored encrypted. It is never shown on this screen and never sent back. " +
          "A key that is protected by a passphrase has to have it taken off first."
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
    ? "Paste the certificate, which starts with -----BEGIN CERTIFICATE-----."
    : "";
}

function checkKeyBlock(value) {
  const text = String(value);

  if (text.indexOf("-----BEGIN") === -1 || text.indexOf("PRIVATE KEY-----") === -1) {
    return "Paste the private key. Its first line is the one that says BEGIN PRIVATE KEY.";
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

  setNotice("The certificate is stored and is in place for every connection made from now on. " +
    "Reload the page to be served it.", "info");

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
    intro.push(statusLine("What this account is called could not be read: " + account.problem,
      "warning"));
  } else {
    intro.push(element("p", "This account is called " + name + "."));
  }

  intro.push(element("p",
    "Fill in the name, the password or both. What is left empty stays as it is."));
  intro.push(element("p",
    "Saving signs out every other client of this account, on this machine and on any " +
      "other, whichever of the two was changed. This one stays signed in. A client that " +
      "is already signed in keeps its session whatever the account is renamed to, so a " +
      "rename that left them alone would change the name and nothing else."));

  const form = buildForm({
    name: "account",
    legend: "Username and password",
    submitLabel: "Save",
    intro: intro,
    fields: [
      {
        name: "username",
        label: "New username",
        note: "Leave it empty to keep the name above."
      },
      {
        name: "current_password",
        label: "Current password",
        type: "password",
        check: function (value) {
          return String(value) === "" ? "Enter the password this account is signed in with." : "";
        },
        note: "The password this account is signed in with. It is asked for on every " +
          "change, a rename included."
      },
      {
        name: "new_password",
        label: "New password",
        type: "password",
        countBytes: true,
        note: "Leave it empty to keep the password. The length is counted in bytes. " +
          "One Hangul syllable counts as three."
      },
      passwordConfirmationField("new_password_confirmation", "New password again", "new_password")
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
    setNotice("Fill in a new username, a new password or both.");

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
    return "The account is changed.";
  }

  const said = [];

  if (data.username_changed) {
    said.push("This account is called " + data.username + " now.");
  }

  if (data.password_changed) {
    said.push("The password is changed. It is the one to sign in with from now on.");
  }

  const ended = data.sessions_ended;

  if (ended === null || ended === undefined || ended === 0) {
    said.push("No other client was signed in.");
  } else {
    said.push(plural(ended, "One other client was signed out.",
      ended + " other clients were signed out."));
  }

  return said.join(" ");
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
  card.appendChild(element("p",
    "Only the settings go back. The registered hosts, the service ports, the account " +
      "and the certificate are left as they are, so nothing has to be registered again " +
      "and you log in with the password you already have."));

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
  card.appendChild(element("h2", "Restart the service"));
  card.appendChild(element("p",
    "A restart is what puts a stored setting that waits for one into place. The API stops " +
      "answering, every tunnel comes down and is built again afterwards, so everything going " +
      "through a tunnel is cut for as long as the restart takes."));

  if (restart.view === null) {
    // What a restart does here could not be read, and the two cases it decides
    // between are not the same press at all: after one the service is back by
    // itself, after the other it stays down. Offering the button without
    // knowing which one this is asks the operator to find out by pressing it.
    card.appendChild(statusLine("What a restart would do could not be read, so it is not " +
      "offered here: " + restart.problem, "warning"));

    return card;
  }

  card.appendChild(element("p", restartOutcome(restart.view, newAddress)));

  const buttons = document.createElement("div");
  buttons.className = "buttons";

  const button = actionButton("Restart", "settings-restart", function () {
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
    const base = "This process runs the program again in place of itself. It keeps the process " +
      "it already is, so the service is back within seconds and nothing has to start it.";

    if (address === null) {
      return base + " It answers at the same address it does now.";
    }

    // A stored setting this restart puts into place moves where the service
    // answers. Saying "the same address" here would be wrong in exactly the
    // case an operator is most likely to be restarting for, and the address
    // they have to go to next is the useful part.
    return base + " It comes back at " + address + ", which is not where this page is, so " +
      "this page goes there rather than waiting here for something that is not coming.";
  }

  return "This platform cannot replace the image of a running process, so the restart ends " +
    "with the process stopped. Whatever supervises the service is what starts it again, and " +
    "a service that was started by hand does not come back at all.";
}

// restartQuestion is what the operator is asked before anything happens. It
// says what is cut either way, and on a platform that does not come back it
// says that too, while there is still something to be done about it.
function restartQuestion(view, newAddress) {
  const address = newAddress === undefined ? null : newAddress;
  const cut = "Restart tunnel-manager? Every tunnel is cut and the API stops answering while " +
    "the service goes down and comes up again.";

  const moved = address === null ? "" : " It comes back at " + address + ", which is what the " +
    "settings waiting for this restart say, so this page goes there once it has had time to " +
    "come up.";

  if (view.comes_back) {
    if (address === null) {
      return cut + " It comes back on its own within seconds, at the same address.";
    }

    return cut + " It comes back on its own within seconds." + moved;
  }

  return cut + " This platform cannot start the program again by itself: bringing it back is " +
    "left to whatever supervises this service, and if it was started by hand it does not come " +
    "back.";
}

async function submitRestart(view, button, newAddress) {
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
    drawRestarting(answer, "The service did not answer again within " + restartPollLimitSec +
      " seconds. Check the server.", newAddress);

    return;
  }

  setNotice("The service is back.", "info");

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
    "The restart was asked for. The service stops answering about " + seconds + " " +
      plural(seconds, "second", "seconds") + " after this screen appeared, every tunnel comes " +
      "down with it and is built again on the way back."));

  nodes.push(element("p", restartOutcome(answer, newAddress)));

  const address = newAddress === undefined ? null : newAddress;

  if (address !== null) {
    // Waiting would be waiting on the wrong address. This page is served from
    // the one being left behind, so asking it again can only ever time out,
    // and reporting that as "it did not come back" would be untrue. It is
    // opened rather than waited on.
    const left = typeof secondsLeft === "number" ? secondsLeft : 0;

    nodes.push(statusLine("The service is not coming back here. This page opens " + address +
      " in " + left + " " + plural(left, "second", "seconds") + ".", "empty"));

    // The link is here so that the wait can be skipped, and so that the
    // address survives if the move does not happen: a page that moved on its
    // own and landed on a certificate warning has still said where it went.
    const now = document.createElement("p");
    const link = document.createElement("a");

    link.href = address;
    link.textContent = address;
    now.appendChild(element("span", "Or open it now: "));
    now.appendChild(link);
    nodes.push(now);
  } else if (problem === "") {
    nodes.push(statusLine("Waiting for the service to answer again. This page asks every " +
      restartPollEverySec + " " + plural(restartPollEverySec, "second", "seconds") +
      " and gives up after " + restartPollLimitSec + " seconds.", "empty"));
  } else {
    nodes.push(statusLine(problem, "warning"));
    nodes.push(element("p",
      "Nothing further happens on this page. Reload it once the service is running again."));
  }

  render("Restarting", nodes);
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
