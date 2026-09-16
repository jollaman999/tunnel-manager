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
  login: { draw: drawLogin },
  setup: { draw: drawSetup }
};

// editingHostID and editingServicePortID say which row has the edit form open.
// Only the identifier is kept: the values in the form come from the last answer
// the list was drawn from, so an edit form never shows a row as it was several
// fetches ago.
let editingHostID = null;
let editingServicePortID = null;

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
        tunnel.status,
        tunnel.server,
        tunnel.local,
        tunnel.remote,
        tunnel.retry_count,
        formatTime(tunnel.last_connected_at),
        tunnel.last_error
      ];
    });

    nodes.push(buildTable(
      ["Host", "Service port", "Status", "Server", "Local", "Remote", "Retries",
        "Last connected", "Last error"],
      rows
    ));
  }

  render("Status", nodes);
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
      hosts.map(hostRow)
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
  }));

  return [
    host.id,
    host.ip,
    host.port,
    host.user,
    host.description,
    host.enabled ? "yes" : "no",
    formatTime(host.updated_at),
    buttons
  ];
}

function hostCreateForm() {
  return buildForm({
    name: "host-create",
    legend: "Add a host",
    submitLabel: "Add",
    fields: [
      { name: "ip", label: "IP" },
      { name: "port", label: "SSH port", value: 22 },
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
      { name: "ip", label: "IP", value: host.ip },
      { name: "port", label: "SSH port", value: host.port },
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
      ports.map(servicePortRow)
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
  }));

  return [
    port.id,
    port.service_ip,
    port.service_port,
    port.local_port,
    port.description,
    formatTime(port.updated_at),
    buttons
  ];
}

function servicePortCreateForm() {
  return buildForm({
    name: "service-port-create",
    legend: "Add a service port",
    submitLabel: "Add",
    fields: [
      { name: "service_ip", label: "Service IP" },
      { name: "service_port", label: "Service port" },
      { name: "local_port", label: "Local port" },
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
      { name: "service_ip", label: "Service IP", value: port.service_ip },
      { name: "service_port", label: "Service port", value: port.service_port },
      { name: "local_port", label: "Local port", value: port.local_port },
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
