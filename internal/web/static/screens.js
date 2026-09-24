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
  update: { label: "nav.update.link", nav: true, draw: drawUpdate },
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

// updateWaitEverySec and updateWaitLimitSec are the same two numbers for the
// wait after an install was started, and they are their own pair because the
// wait is a different one.
//
// A restart is a process letting go of a port and taking it again. An install
// is a release being fetched over the network, checked, written into place, and
// only then a restart. The fetch alone is allowed two minutes by the end that
// does it (internal/install/fetch.go, fetchTimeout), and the service is still
// answering for all of it: it is stopped after the file is in hand. So a page
// that gave up at two minutes would reload in the one window where the server
// is not there to answer, and an operator would be shown a browser error for an
// install that was going fine.
//
// Three minutes is past the fetch and past the restart that follows it. It
// costs nothing when things go well, because the wait ends as soon as a new
// version answers rather than when the clock runs out.
//
// The ask is every two seconds rather than every second. Nothing is waiting on
// the first moment it could be noticed, and the far side is a service that is
// busy being installed.
const updateWaitEverySec = 2;
const updateWaitLimitSec = 180;

// editingHostID and editingServicePortID say which row has the edit form open.
// Only the identifier is kept: the values in the form come from the last answer
// the list was drawn from, so an edit form never shows a row as it was several
// fetches ago.
let editingHostID = null;
let editingServicePortID = null;

// pickedFlipRefusals are the Hosts the last press on one of the two flips could
// not change, each with what the server said about that one. It is held out
// here because the press ends by drawing the list again, and a run of a hundred
// can be refused a row at a time and for a different reason each time: the
// counts fit on the line above the screen and the reasons do not.
//
// It is read by the draw that follows the press and emptied by it, so what is
// on the screen is always the last press and never the one before it. Turning
// the page or opening an edit form takes it off, which is what the reasons are
// worth by then: they were about rows that were ticked at the time of a press
// that has since been read.
let pickedFlipRefusals = [];

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
//
// A window is put on one of these by pageControls the first time the run of
// page numbers is pushed along by hand, and it is for the same reason: it is
// how the numbers around page eighteen are reached from page three, and a run
// that came back to page three every five seconds would never be pressed. It
// is written where the page is rather than beside it because the two panels
// carry a page of their own; see listNumberFirst for what it holds.
const listPages = {
  status: { number: 1, size: listSizes[0] },
  hosts: { number: 1, size: listSizes[0] },
  "service-ports": { number: 1, size: listSizes[0] }
};

// listPicks is what has been ticked on the lists that can be ticked. It is
// held out here for the reason listPages is: the screens are drawn again from
// scratch every five seconds, and ticks kept inside the draw would be gone by
// the time the operator reached the press they were ticked for.
//
// The local forwards of a Host are in a panel and not on a screen, and their
// entry is emptied each time the panel opens: the ticks of the last one were
// for the rows of another Host.
//
// The rule for reading it: the keys of listPicks.hosts are the Hosts ticked on
// the Host list and the keys of listPicks["service-ports"] are the service
// ports ticked on the service port list, each an identifier written as a
// string and each held against true. pickedIDs hands the same thing back as
// numbers, which is what a request wants. Nothing else may be under a key: a
// row that is not ticked is deleted rather than held against false, so the
// count of what is ticked is the count of the keys.
//
// Two things narrow what can be in there, and both are done in keepPicksOnPage
// as the list is drawn rather than left to the press to deal with:
//
//   - Only rows of the page on screen. A tick is for a row the operator is
//     looking at, and a press that acted on more than that would reach rows
//     that were never seen. Turning the page therefore clears the ticks, which
//     is what leaves the tick in the head of the table clear on the new page.
//   - Only rows that still exist. A row deleted from another screen, or by
//     somebody else, is gone from the next answer and goes from here with it.
const listPicks = {
  hosts: {},
  "service-ports": {},
  "local-forwards": {}
};

// pickedIDs is what a press that acts on the ticks reads: the identifiers
// ticked on one list, as numbers, smallest first. The order is fixed so that a
// run of requests goes out in the order the rows are read in, and so that what
// is shown back to the operator is in that order too.
function pickedIDs(name) {
  return Object.keys(listPicks[name])
    .map(Number)
    .sort(function (one, other) {
      return one - other;
    });
}

// keepPicksOnPage drops every tick that is not on the page of rows just
// fetched. It is the same treatment the row with the edit form open is given a
// few lines below every call of it: what the screen is holding the identifier
// of has to be something in the answer that screen was drawn from, or it is
// holding a row that is not there.
function keepPicksOnPage(name, items) {
  const here = {};

  for (const item of items) {
    here[String(item.id)] = true;
  }

  for (const id of Object.keys(listPicks[name])) {
    if (!here[id]) {
      delete listPicks[name][id];
    }
  }
}

// listPickColumn makes the column of ticks for one list: the one in the head of
// the table, which takes and releases the whole page, and the one of each row.
//
// They are made together because each is the answer to the other. The head is
// ticked exactly while every row of the page is ticked, so a row that is
// released clears it and the last row that is ticked sets it; and the head
// takes the rows of this page only, which is the rule listPicks is kept under.
//
// A press on either changes listPicks and the ticks already on screen, and
// fetches nothing. The list is read again five seconds later whatever happens,
// and that draw puts the same ticks back from listPicks.
//
// describe is what a reader who cannot see the column is told a row's tick is
// for. A box on its own is read out as a box, and there are ten of them.
function listPickColumn(name, items, describe) {
  const picked = listPicks[name];
  const boxes = [];
  const head = document.createElement("input");

  head.type = "checkbox";
  head.dataset.field = name + "-pick-all";
  head.setAttribute("aria-label", t("list.pick-all.aria"));
  head.checked = items.length > 0 && items.every(function (item) {
    return Object.prototype.hasOwnProperty.call(picked, String(item.id));
  });

  function hold(id, on) {
    if (on) {
      picked[String(id)] = true;
    } else {
      delete picked[String(id)];
    }
  }

  head.addEventListener("change", function () {
    for (const one of boxes) {
      one.box.checked = head.checked;
      hold(one.id, head.checked);
    }
  });

  return {
    head: head,
    box: function (id) {
      const box = document.createElement("input");

      box.type = "checkbox";
      box.dataset.field = name + "-pick-" + id;
      box.setAttribute("aria-label", describe(id));
      box.checked = Object.prototype.hasOwnProperty.call(picked, String(id));
      box.addEventListener("change", function () {
        hold(id, box.checked);
        head.checked = boxes.every(function (one) {
          return one.box.checked;
        });
      });

      boxes.push({ box: box, id: id });

      return box;
    }
  };
}

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

// settingsImportResult is what the last import of the settings of the manager
// did not take from the file, for the reason transferResult is kept. It is null
// when that import took all of it, and the card then says nothing more.
let settingsImportResult = null;

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
// server, so nothing here has to be cleared but the language the session gave
// access to: the login is drawn in what the browser asks for, as it is in a
// tab that has never signed in.
async function logOut() {
  await apiCall("POST", "/api/logout");
  await forgetInstallationLang();

  // Said on the window rather than carried to the login as a line above it.
  // Signing out is something that went through, and the screen it lands on is
  // one the reader has business with: a line over the login box is a line in
  // the way of the next thing they came to do.
  setToast(function () {
    return t("login.signed-out.notice");
  });

  navigate("login");
}

// drawLogin is the screen a client without a session lands on.
//
// One form covers both states of the account, because nothing the server
// answers before a login says which state it is in. The username is ignored
// while the account is still to be set up, so sending it empty is right then
// and sending it filled in is right afterwards.
async function drawLogin() {
  // Whether the setup is done decides whether the hint under the username
  // box is still true. The server is asked, since it is the one that knows;
  // a server that cannot say, an older one for instance, leaves the hint in,
  // which is the safe way round.
  let setupDone = false;

  try {
    const state = await apiCall("GET", "/api/setup");

    setupDone = state !== null && state.setup_required === false;
  } catch (error) {
    setupDone = false;
  }

  const form = buildForm({
    name: "login",
    legend: t("login.form.title"),
    submitLabel: t("login.submit.button"),
    fields: [
      {
        name: "username",
        label: t("login.username.label"),
        // The hint says to leave the box empty before the setup, and is
        // left out once the setup is done.
        note: setupDone ? undefined : t("login.username.hint")
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
      say: function () {
        return t("setup.needed.notice");
      },
      kind: "info"
    });

    return;
  }

  // The language the installation draws a browser that has picked none in is
  // read behind the login and nowhere else, so this is the first moment it can
  // be had. It is read before the move, so the screen moved to is the first one
  // drawn in it.
  await followInstallationLang();

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
      const said = sayOf(error);

      // A refusal, and said as one: on the window so that it is seen at the
      // moment the screen changes under the reader, and above the login so
      // that it is still there to read afterwards. setFailure is not what puts
      // the line up here, because navigate writes the line itself and would
      // write over one set before it.
      showToast(said, "error");
      navigate("login", { say: said, kind: "error" });

      return;
    }

    throw error;
  }

  // Said on the window, as signing out is. The account being set up is
  // something that went through, and the screen it lands on is the one the
  // reader came here to get to.
  setToast(function () {
    return t("setup.done.notice");
  });

  navigate("status");
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

  // The counts are of everything there is and not of the page below them. They
  // are what says what the installation is doing, and a count that followed the
  // page would read as a tunnel count that fell to ten.
  //
  // There are two sets of three because the table below holds two sorts of row
  // and the two are not added up anywhere. A single set over both would say
  // that nine of eleven are connected without saying which sort the two that
  // are not belong to, and the sorts fail for different reasons and are fixed
  // in different places. Each label names its sort for the same reason: three
  // numbers headed Desired, Rows and Connected over a table of both sorts read
  // as being about all of it, which is what they are no longer about.
  const counts = document.createElement("div");
  counts.className = "counts";
  counts.appendChild(countBox(t("status.desired.label"), data.desired_tunnels, "desired"));
  counts.appendChild(countBox(t("status.rows.label"), data.total_tunnels, "total"));
  counts.appendChild(countBox(t("status.connected.label"), data.connected_tunnels, "connected"));
  counts.appendChild(countBox(t("status.forwards-desired.label"),
    data.desired_local_forwards, "forwards-desired"));
  counts.appendChild(countBox(t("status.forwards-rows.label"),
    data.total_local_forwards, "forwards-total"));
  counts.appendChild(countBox(t("status.forwards-connected.label"),
    data.connected_local_forwards, "forwards-connected"));

  const nodes = [counts];

  // What is waiting to be answered goes above the sentences about the counts.
  // Every other line on this screen is about something to fix on a machine or
  // on the way to it; this one is a question that stays where it is until
  // somebody settles it, and the tunnels it is about are not running until
  // they do.
  const asked = hostKeysNotice(data);
  if (asked !== null) {
    nodes.push(asked);
  }

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

  // A local forward is not asked the first of those two questions. Its row is
  // the one that was configured and is there whether or not anything runs, so
  // there is no row that has yet to be written for a line to be about; what a
  // forward can be short of is the running, and that is what this says.
  const forwardsDown = data.desired_local_forwards - data.connected_local_forwards;
  if (forwardsDown > 0) {
    nodes.push(statusLine(
      t(plural(forwardsDown, "status.forwards-down-one.notice",
        "status.forwards-down-many.notice"), { count: forwardsDown }),
      "warning"
    ));
  }

  // Nothing at all is not the same as everything connected. An installation
  // with no Host or no service port has no tunnel for the line to be about,
  // and the empty list under it is what says so.
  //
  // The count of what should be running is the only one worth asking. The two
  // lines above have already left, so the rows are as many as should be running
  // and the connected ones are as many as there are rows: where that number is
  // nought, all three are.
  //
  // A local forward that is down keeps the line off the screen even though the
  // line speaks of tunnels alone. It is drawn in the colour of everything being
  // well, and that colour over a warning about a forward under it is the screen
  // saying two things at once.
  if (data.desired_tunnels > 0 && missing === 0 && down === 0 && !(forwardsDown > 0)) {
    nodes.push(statusLine(t("status.all-connected.notice"), "ok"));
  }

  const tunnels = data.tunnels === null || data.tunnels === undefined ? [] : data.tunnels;

  if (tunnels.length === 0) {
    nodes.push(statusLine(t("status.no-tunnels.empty"), "empty"));
  } else {
    const rows = tunnels.map(function (tunnel) {
      const forward = isLocalForward(tunnel);
      const cells = [
        tunnel.host_id,
        forward ? t("status.kind-local-forward.text") : t("status.kind-service-port.text"),
        // A local forward is carried by no service port, and the column holds
        // a dash rather than a blank: a blank cell in a column of numbers
        // reads as a number that failed to come through.
        forward ? "-" : tunnel.sp_id,
        statusBadge(tunnel.status),
        tunnel.server,
        openedCell(tunnel),
        tunnel.remote,
        // Nothing measures the reach of a local forward, so the cell is left
        // empty. reachBadge draws an unmeasured reading as "unknown", which is
        // what a tunnel nothing has asked about yet says; on a forward that
        // would be the screen promising a reading that is never taken.
        forward ? "" : reachBadge(tunnel.forward_reach),
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

      // Nothing is said under a tunnel the host key check refused, which is
      // what used to go here first. A Host with four service ports carries four
      // such tunnels, all refused by the one key, so the notice was the same
      // question drawn four times over and a Host whose tunnels sit on a later
      // page asked it nowhere. The row still says which state it is in through
      // its badge, and the question is asked once, over the table.

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

      const denied = forwardAdvice(tunnel);
      if (denied !== null) {
        under.push(denied);
      }

      const addresses = forwardAddresses(tunnel);
      if (addresses !== null) {
        under.push(addresses);
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

    // The pages are cut from both sorts together, so what the controls are
    // built over is the count of rows and not the count of tunnels. Over the
    // tunnels alone the last pages of an installation that has local forwards
    // are pages the numbering does not reach.
    const controls = pageControls("status", page, data.total_rows, drawStatus);
    if (controls !== null) {
      nodes.push(controls);
    }

    nodes.push(buildTable(
      [t("status.host.column"), t("status.kind.column"), t("status.service-port.column"),
        t("status.status.column"), t("status.server.column"), t("status.opened.column"),
        t("status.reaches.column"), t("status.port-reached.column"),
        t("status.retries.column"), t("status.last-connected.column")],
      rows,
      [0, 2, 8]
    ));
  }

  render(t("status.screen.title"), nodes);
}

// isLocalForward is which of the two sorts a status row is. The server says it
// on kind, and it is asked for by name rather than guessed at from the row: a
// local forward and a service port tunnel carry the same fields with the same
// addresses in them, and the only thing that tells them apart is this word.
function isLocalForward(row) {
  return row.kind === "local_forward";
}

// openedCell is the address in the Opened column, with the machine the port is
// open on written beside it.
//
// The machine is in the cell and not in the heading because the two sorts of
// row open their port on different machines: a service port tunnel opens it on
// the Host and a local forward opens it here. Both addresses are written the
// same way, 0.0.0.0:9000 and the like, so a column headed Local held one of
// each and nothing said which was which. The column next to it is the mirror
// of this one - a tunnel reaches its service from here, a forward reaches its
// target from the Host - and it carries no machine of its own, because a row
// that says where it was opened has said which way round it runs.
function openedCell(row) {
  const address = row.local === null || row.local === undefined ? "" : String(row.local);

  // A row with no address carries none. There is nothing to say a machine
  // about, and a cell reading "Host" alone would name a port that is not there.
  if (address === "") {
    return "";
  }

  return isLocalForward(row)
    ? t("status.opened-here.text", { address: address })
    : t("status.opened-host.text", { address: address });
}

// statusBadge is what a tunnel is, drawn so that the one row that is not
// working is found without reading the column. A state the server is known to
// send is drawn as a word in the language of the page, and one added later is
// drawn as the server said it, so it still shows up; only the three that are
// known are coloured. data-status carries the word as the server said it in
// every case, since that is what the styles and the recorder pick a row by.
function statusBadge(status) {
  const known = { connected: "ok", error: "bad", reconnecting: "waiting" };
  const words = {
    starting: "status.state-starting.text",
    connected: "status.state-connected.text",
    reconnecting: "status.state-reconnecting.text",
    error: "status.state-error.text",
    stopped: "status.state-stopped.text",
    disabled: "status.state-disabled.text",
    off: "status.state-off.text"
  };
  const text = status === null || status === undefined ? "" : String(status);

  // The two the host key check leaves behind carry a word of their own as well
  // as a colour of their own. They are the states an operator answers, so what
  // they say is a sentence rather than a name out of the database.
  const asked = hostKeyState(text);
  const word = asked !== null
    ? t(asked.word)
    : Object.prototype.hasOwnProperty.call(words, text) ? t(words[text]) : text;
  const badge = element("span", word);

  // Asked of the table itself and not of what every object inherits, so that a
  // status the server names "constructor" is coloured as unknown rather than
  // with the name of a function.
  const paint = Object.prototype.hasOwnProperty.call(known, text) ? known[text] : "unknown";

  badge.className = "badge " + (asked === null ? paint : asked.paint);
  badge.dataset.status = text;

  return badge;
}

// reachBadge is whether the forwarded port answered a connection opened by
// tunnel-manager. A reading the server is known to send is drawn in the language
// of the page and one added later as the server sent it, the way the status
// beside it is, so it still shows up. A row that carries none, which is one
// written before the reading existed, reads as not measured rather than as an
// empty cell. data-reach keeps the word the server sent.
function reachBadge(reach) {
  const words = {
    reachable: "status.reach-reachable.text",
    unreachable: "status.reach-unreachable.text",
    unknown: "status.reach-unknown.text"
  };
  const said = reach === null || reach === undefined ? "" : String(reach);
  const text = said === "" ? "unknown" : said;
  const badge = element("span",
    Object.prototype.hasOwnProperty.call(words, text) ? t(words[text]) : text);

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

  // Windows is asked about before OpenSSH, because the build that runs there
  // calls itself OpenSSH too and the two are not the same machine to advise
  // about: there is no privileged port on Windows, so nothing there is opened
  // only by an administrator.
  if (said.indexOf("openssh_for_windows") !== -1 || said.indexOf("windows") !== -1) {
    return "windows";
  }

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

// forwardAdvice is what is said under a tunnel whose forwarded port the SSH
// server would not open. It is the other half of reachAdvice above: that one is
// for a port that was opened and cannot be reached, this one for a port that
// was never opened.
//
// It names no cause. The refusal carries no reason, and the several settings
// that produce it look identical from here, so what is offered is the list of
// them with the likeliest first. Which one goes first is decided on what this
// end does know: which server refused, and which port it was asked for.
function forwardAdvice(tunnel) {
  if (tunnel.error_kind !== "forward_denied") {
    return null;
  }

  const kind = sshServerKind(tunnel.server_banner);
  const port = forwardedPort(tunnel.local);
  const box = document.createElement("div");

  box.className = "reach-advice";
  box.dataset.forwardAdvice = kind;

  box.appendChild(element("strong", t("status.denied.text")));
  box.appendChild(element("p", t("status.denied-cause.text")));

  const causes = [];

  // A Windows Host is told nothing about privileged ports, which it does not
  // have. Saying it there would send the operator looking for an account that
  // would change nothing.
  if (kind !== "windows" && port !== null && port < 1024) {
    causes.push(t("status.denied-privileged.text", { port: String(port) }));
  }

  causes.push(t("status.denied-forwarding.text"));
  causes.push(t("status.denied-permitlisten.text"));
  causes.push(t("status.denied-authorized-keys.text"));
  causes.push(t("status.denied-in-use.text"));

  box.appendChild(bulletList(causes));

  return box;
}

// forwardAddresses is what is said under a connected tunnel about the addresses
// of its forwarded port: the ones that were asked for, the one that answered a
// connection opened from here, the ones the Host itself named, and nothing
// else.
//
// It is drawn as a note and not as a warning. None of it is something to go and
// fix: a tunnel whose ports were asked for on the Host itself has no address
// this machine can try, and that is the scope doing what it was picked for. The
// one thing on this screen that is a warning about reach is what reachAdvice
// draws, for a port that was tried and gave nothing back.
//
// What the reading of the requests is not is a list of what is open. It is what
// the SSH server answered to each tcpip-forward request, and an answer is not a
// binding: a server set to bind every interface takes both families on the
// first request and says no to the second, and it does that for a request that
// named the loopback address too. So the line says what was asked, what was
// answered and what was confirmed by a connection, and it never calls an answer
// an open address.
//
// The one thing here that does say which addresses are open is the last line,
// and it is there because the Host was asked and answered. It is drawn only
// where there is an answer, so the rest of the box is what is said about every
// forward and that line is what is said about the ones that could be asked
// about.
//
// Nothing is drawn where the row carries no reading, which is a tunnel nothing
// has been asked of yet and a row stored before there was a column. A line
// drawn from an absence of readings would be on every row of an installation
// that has just been upgraded.
function forwardAddresses(tunnel) {
  const reach = typeof tunnel.open_reach === "string" ? tunnel.open_reach : "";

  // Only under a tunnel that is up. The readings belong to the connection that
  // stands, and a row that is reconnecting or in error carries what the last
  // connection left, which is not a fact about the tunnel as it is now.
  if (tunnel.status !== "connected") {
    return null;
  }

  if (reach !== "both" && reach !== "ipv4" && reach !== "ipv6") {
    return null;
  }

  // Nothing is drawn where nothing came out other than what was asked for.
  //
  // The box is four sentences, and on a forward that went up the way it was
  // asked to every one of them says so: this was asked for, the server agreed,
  // a connection from here arrived, the Host is listening on those addresses.
  // An operator reading a screen of tunnels does not need that under each of
  // them, and a box that is always there is one nobody reads on the row where
  // it says something.
  if (nothingCameOutOfTheOrdinary(tunnel, reach)) {
    return null;
  }

  const box = document.createElement("div");

  box.className = "forward-addresses";
  box.dataset.openReach = reach;

  box.appendChild(element("strong", t("status.addresses-known.text")));
  box.appendChild(element("p", askedSentence(tunnel.local)));
  box.appendChild(element("p", answeredSentence(reach)));

  // What an answer is worth is said where one of the two requests was turned
  // down, which is where a reader is most likely to take the answer for a
  // measurement.
  if (reach !== "both") {
    box.appendChild(element("p", t("status.addresses-answer-no-proof.text")));
  }

  box.appendChild(element("p", confirmedSentence(tunnel)));

  const listening = listeningSentence(tunnel.listen_addresses);
  if (listening !== null) {
    box.appendChild(element("p", listening));
  }

  return box;
}

// nothingCameOutOfTheOrdinary says whether the forward is open exactly as it
// was asked to be, with nothing about it left to tell.
//
// Three things could differ and none of them does here. The SSH server said yes
// to both of the addresses that were asked for. The Host, asked what it has
// open on that port, named those same addresses. And the port answered a
// connection opened from here, or was never one this end could dial.
//
// The middle one is the one worth drawing when it differs, and it differs more
// often than it sounds: a server set to bind every interface ignores a request
// for the loopback and opens the port to its whole network, which is the
// opposite of what was chosen and is not visible anywhere else.
//
// A Host that said nothing is not agreement. It leaves the answer unknown, and
// unknown on a forward whose requests were both agreed to and which answered a
// connection is not something to put on the screen: it is the ordinary state of
// a Host this end cannot ask.
function nothingCameOutOfTheOrdinary(tunnel, reach) {
  if (reach !== "both") {
    return false;
  }

  if (tunnel.forward_reach === "unreachable") {
    return false;
  }

  return listeningIsWhatWasAsked(tunnel.local, tunnel.listen_addresses);
}

// listeningIsWhatWasAsked compares what the Host named against the pair the
// scope names. A Host that named nothing is not a difference, and neither is a
// row whose address is none of the four a scope is made of: what is being
// looked for is a Host that named something else.
function listeningIsWhatWasAsked(local, listening) {
  const pair = bindScopePairOf(local);

  if (pair === null) {
    return true;
  }

  if (typeof listening !== "string" || listening === "") {
    return true;
  }

  const named = listening.split(",").map(function (one) {
    return one.trim();
  }).filter(function (one) {
    return one !== "";
  });

  if (named.length === 0) {
    return true;
  }

  // The addresses are compared as a set. The Host writes them in whatever order
  // its own table had them, which is not an order this end chose and not one a
  // difference should be read out of.
  const asked = [pair.v4, pair.v6].sort().join(",");

  return named.slice().sort().join(",") === asked;
}

// listeningSentence is what the Host said is listening on the forwarded port,
// and null where it said nothing.
//
// It comes last because it is the strongest thing in the box. Everything above
// it is what was asked for and what was answered to the asking, and this is the
// machine the port is on being asked what it has open.
//
// Nothing is drawn where the row carries no addresses, and no reason is given
// for that. An account with no shell refuses the session, a system with neither
// ss nor netstat prints nothing that can be read, and a row stored before there
// was a column carries nothing either; none of those is told apart from here,
// and a sentence naming one of them would be a guess. What stays on screen then
// is the sentence above about what nothing here can confirm.
function listeningSentence(listening) {
  if (typeof listening !== "string") {
    return null;
  }

  // A field that is empty is dropped rather than drawn: a value that is one
  // separator on its own splits into two empty pieces, and neither is an
  // address.
  const addresses = listening.split(",").filter(function (address) {
    return address !== "";
  });

  if (addresses.length === 0) {
    return null;
  }

  return t("status.addresses-listening.text", { addresses: addresses.join(", ") });
}

// askedSentence names the addresses the forwarded port was asked to be opened
// at. They are the pair the bind scope names, which is what the address in the
// row is one of; where that address is none of the four a scope is made of, the
// row is all there is to name and the sentence names it alone.
function askedSentence(local) {
  const pair = requestedPair(local);

  if (pair === null) {
    return t("status.addresses-asked-one.text", { address: typeof local === "string" ? local : "" });
  }

  return t("status.addresses-asked-pair.text", { first: pair.v4, second: pair.v6 });
}

// answeredSentence is what the SSH server said to the two requests. It is an
// answer and not a measurement, which is what the sentence under it says where
// one of the two was turned down.
function answeredSentence(reach) {
  if (reach === "ipv4") {
    return t("status.addresses-answered-ipv4.text");
  }

  if (reach === "ipv6") {
    return t("status.addresses-answered-ipv6.text");
  }

  return t("status.addresses-answered-both.text");
}

// confirmedSentence is what was confirmed by a connection opened from here,
// which is the one evidence this end can hold that a port is up.
//
// A forward asked for on the Host itself has nothing here that can be tried, so
// the empty answer for one of those is what it is meant to be rather than
// something missing: the sentence says so, so that a scope doing what it was
// picked for does not read as a tunnel with something wrong with it.
function confirmedSentence(tunnel) {
  if (tunnel.forward_reach === "reachable") {
    const answered = probedAddress(tunnel.server, tunnel.local);

    // Where the two columns cannot be cut into a host and a port, the address
    // is left out rather than written wrong, which is what reachAdvice does
    // with the same pair of columns.
    return answered === ""
      ? t("status.addresses-confirmed-plain.text")
      : t("status.addresses-confirmed.text", { address: answered });
  }

  if (requestedScope(tunnel.local) === bindScopeLoopback) {
    return t("status.addresses-host-only.text");
  }

  return t("status.addresses-confirmed-none.text");
}

// requestedHost is the address out of the local column, without the port. An
// IPv6 address is written in brackets there, so it is taken from between them;
// everything else is cut at the last colon, which is what separates a port from
// an address that has none of its own.
function requestedHost(local) {
  if (typeof local !== "string") {
    return "";
  }

  if (local.startsWith("[")) {
    const close = local.indexOf("]");

    return close < 0 ? "" : local.slice(1, close);
  }

  const at = local.lastIndexOf(":");

  return at < 0 ? "" : local.slice(0, at);
}

// bindScopeWildcard and bindScopeLoopback are the two answers an assignment can
// carry, spelled the way the API spells them. An assignment that carries
// neither holds the empty value, and that means the wildcard: it is what every
// row written before there was a column is already running on, and nothing here
// may quietly move one of those somewhere narrower.
const bindScopeWildcard = "wildcard";
const bindScopeLoopback = "loopback";

// The two addresses of each bind scope, one per family, spelled the way the
// server asks for them. They are here so that a row, which carries one address,
// can name the pair that was asked for: the scope itself is not on the row.
const bindScopePairs = {
  "0.0.0.0": { scope: bindScopeWildcard, v4: "0.0.0.0", v6: "::" },
  "::": { scope: bindScopeWildcard, v4: "0.0.0.0", v6: "::" },
  "127.0.0.1": { scope: bindScopeLoopback, v4: "127.0.0.1", v6: "::1" },
  "::1": { scope: bindScopeLoopback, v4: "127.0.0.1", v6: "::1" }
};

// bindScopePairOf is the pair the address in the local column belongs to, and
// null for an address that is none of the four. Asked of the table itself and
// not of what every object inherits, so that a row naming "constructor" is an
// address nothing is known about rather than a function.
function bindScopePairOf(local) {
  const host = requestedHost(local);

  return Object.prototype.hasOwnProperty.call(bindScopePairs, host) ? bindScopePairs[host] : null;
}

// requestedScope is which scope the forwarded port was asked for on, out of the
// address the row names, and the empty string where that address is none of the
// four a scope is made of.
function requestedScope(local) {
  const pair = bindScopePairOf(local);

  return pair === null ? "" : pair.scope;
}

// requestedPair is the two addresses that were asked for, port and all, and
// null where the address in the row is none of the four. The port is the one
// the row carries, which is the port the SSH server confirmed.
function requestedPair(local) {
  const pair = bindScopePairOf(local);
  const port = forwardedPort(local);

  if (pair === null || port === null) {
    return null;
  }

  return { v4: pair.v4 + ":" + String(port), v6: "[" + pair.v6 + "]:" + String(port) };
}

// forwardedPort is the port the Host was asked to open, out of the local
// column. It is cut at the last colon, which is what separates a port from an
// address that has colons of its own, and a value that does not read as a port
// gives nothing back so that the advice is written without it.
function forwardedPort(local) {
  if (typeof local !== "string") {
    return null;
  }

  const at = local.lastIndexOf(":");
  if (at < 0) {
    return null;
  }

  const port = Number(local.slice(at + 1));

  return Number.isInteger(port) && port > 0 && port <= 65535 ? port : null;
}

// hostKeyState is what a status the host key check left behind is drawn as,
// and null for every other status.
//
// The two are apart from the rest because they are not something to fix on
// the Host or on the way to it. They are a question about which machine is
// answering, which only a person can settle, so each carries a word of its
// own and the colour of how much is at stake: a Host that has never been
// approved is a step of registering it and is drawn as something waiting,
// while a key that changed under a Host that was approved is either a rebuilt
// server or a connection that is not reaching the server at all, and that one
// is drawn the way a failure is.
function hostKeyState(status) {
  const states = {
    host_key_unapproved: {
      word: "status.host-key-unapproved.text",
      said: "status.host-key-unapproved.notice",
      paint: "waiting"
    },
    host_key_mismatch: {
      word: "status.host-key-mismatch.text",
      said: "status.host-key-mismatch.notice",
      paint: "bad"
    }
  };

  const text = status === null || status === undefined ? "" : String(status);

  // Asked of the table itself, for the reason statusBadge asks that way.
  return Object.prototype.hasOwnProperty.call(states, text) ? states[text] : null;
}

// hostKeysNotice is the one line over the table about the Hosts that are
// waiting for a host key to be approved: how many there are, and the press that
// opens the list of them.
//
// It is drawn from the two counts of the answer and not from the rows below it.
// The counts are over every Host the installation has, the way the three at the
// top are over every tunnel, and the rows are one page of tunnels: a Host with
// four service ports carries four refused tunnels, so a notice per row asked
// the same question four times, and a Host whose tunnels are all on a later
// page asked it nowhere.
//
// There is one line and not two, because what the two counts are is two
// different questions with the same answer: the press under them. What the
// second one changes is the colour, since a key that changed under a Host that
// was approved is either a rebuilt server or a connection that is not reaching
// the server at all, and that is not the colour of a Host waiting to be
// registered.
//
// One Host waiting is said differently from several. The sentences that say
// what each state means are written about one Host - "this host" - so they are
// what a single one is said with, and that is the state an installation that
// answers these as they arrive is usually in. Where there are several it is the
// two counts, and what one of them means is on the panel a row of the list
// opens, beside the Host it is about.
function hostKeysNotice(data) {
  const mismatched = countOf(data, "host_keys_mismatched");
  const waiting = countOf(data, "host_keys_unapproved") + mismatched;

  if (waiting === 0) {
    return null;
  }

  const state = hostKeyState(mismatched > 0 ? "host_key_mismatch" : "host_key_unapproved");
  const box = document.createElement("div");

  box.className = "host-key-notice " + state.paint;
  box.dataset.hostKey = mismatched > 0 ? "host_key_mismatch" : "host_key_unapproved";

  box.appendChild(element("p", t(
    plural(waiting, "status.host-keys-waiting-one.notice", "status.host-keys-waiting-many.notice"),
    { count: waiting }
  )));

  if (waiting === 1) {
    box.appendChild(element("p", t(state.said)));
  } else if (mismatched > 0) {
    box.appendChild(element("p", t(
      plural(mismatched, "status.host-keys-changed-one.notice",
        "status.host-keys-changed-many.notice"),
      { count: mismatched }
    )));
  }

  box.appendChild(actionButton(t("status.host-key-review.button"), "host-keys",
    openHostKeysPanel));

  return box;
}

// openHostKeysPanel is the list of the Hosts that have a host key waiting to be
// approved, and where several of them are answered in one press.
//
// It is a panel and not a part of the status screen because of what an upgrade
// looks like. Every Host registered before the host key check existed is
// waiting for a first approval at the same moment, so on an installation of two
// hundred Hosts the list is two hundred rows long, and two hundred rows laid
// over the table would be the screen. Over the table there is one line saying
// how many there are; the list itself is here, a page at a time.
//
// The fingerprint of every Host is in its row. What a tick means is that the
// operator compared that fingerprint with the server, and a fingerprint that
// has to be opened to be seen is one that nobody opens: the row would be ticked
// on the identifier of the Host alone, which is the one thing about it that was
// never in question.
async function openHostKeysPanel() {
  // The panel has a page of its own and does not touch listPages. It is opened
  // and closed while the table behind it stays on the page it was left on, and
  // the two lists are not the same length anyway.
  const page = { number: 1, size: listSizes[0] };

  // What is ticked, held across the pages rather than read off the boxes of the
  // page that happens to be drawn. The work this panel is for is twenty pages
  // of ticking on an installation of two hundred, and a selection that a turn
  // of the page threw away would be twenty presses of approve, which is the
  // thing the panel is here to save.
  //
  // What is held is the row and not the identifier, because the approval names
  // the fingerprint of each Host and the confirmation lists them: both are of
  // the page a Host was ticked on and not of the page on the screen.
  const picked = {};

  // Why a Host of the last press was not approved, kept beside the list so that
  // the row it belongs to carries it. A refusal is per Host - the key changed
  // under that one, or it was approved from somewhere else - and the answer is
  // the only place it is said.
  const refusals = {};

  const list = document.createElement("div");

  list.className = "host-key-list";
  list.dataset.list = "host-keys";

  // Why a press or a page was refused. It is shown inside the panel because the
  // line above the screen is behind the backdrop, where the operator who
  // pressed the button cannot read it.
  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "host-keys";
  problem.hidden = true;

  // How many are ticked, under the list and not in it. What is about to be sent
  // is mostly not on the screen, since it was ticked on pages that have been
  // turned away from, and this is the one thing that says so before the
  // confirmation does.
  const chosen = element("p", "");

  chosen.className = "host-keys-chosen";
  chosen.dataset.chosen = "host-keys";

  function pickedList() {
    return Object.keys(picked).map(function (key) {
      return picked[key];
    });
  }

  // approveButton is the press along the bottom of the panel. It is looked up
  // when it is wanted rather than kept, because the list is drawn once before
  // there is a panel to hold it and the lookup then has nothing to find.
  function approveButton() {
    return document.querySelector(
      "[data-modal-panel=\"host-keys\"] [data-action=\"host-keys-approve\"]"
    );
  }

  // The count and the press under it are about a list with something in it. A
  // Host approved from its own row empties the list while the panel is up, and
  // what would be left is a press that sends nothing and a line counting to
  // nothing, over a list saying there is nothing here to approve.
  function sayChosen() {
    const count = pickedList().length;
    const empty = shown.length === 0;
    const approve = approveButton();

    chosen.hidden = empty;
    chosen.textContent = t(plural(count, "status.host-keys-chosen-one.text",
      "status.host-keys-chosen-many.text"), { count: count });

    if (approve !== null) {
      approve.hidden = empty;
    }
  }

  // The rows of the page that is drawn, and how long the whole list is. They
  // are kept so that ticking the box that takes the whole page can draw the
  // list again without asking the server for a page it already has.
  let shown = [];
  let total = 0;

  function drawList() {
    // Only the list is built again. The panel around it is the one openModal
    // put up, and nothing here writes to #app, so a draw of the screen behind
    // the backdrop cannot take the panel down and this cannot draw over it.
    list.textContent = "";

    if (shown.length === 0) {
      list.appendChild(statusLine(t("status.host-key-gone.text"), "empty"));
      sayChosen();

      return;
    }

    const controls = pageControls("host-keys", page, total, turnPage);
    if (controls !== null) {
      list.appendChild(controls);
    }

    const all = hostKeysPageBox(shown, picked, function () {
      drawList();
    });

    if (all !== null) {
      list.appendChild(all);
    }

    for (const item of shown) {
      list.appendChild(hostKeyRow(item, picked, refusals[String(item.host_id)], sayChosen,
        function () {
          return approveOne(item);
        }));
    }

    sayChosen();
  }

  async function drawPage() {
    const answer = await apiCall("GET", "/api/host-key?" + pageQuery(page));

    takeListPage(page, answer);

    shown = answer === null || answer.items === null || answer.items === undefined
      ? []
      : answer.items;
    total = answer === null || typeof answer.total !== "number" ? shown.length : answer.total;

    drawList();
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

  // approveOne is the press on a row, and it is the panel this screen has
  // always had: the Host is read again, the fingerprints are put side by side,
  // and a key that replaces a trusted one asks for the password of the account.
  // There is no confirmation over it, because one Host approved from its own
  // row is the thing being confirmed.
  async function approveOne(item) {
    const approved = await openHostKeyPanel(item.host_id);
    if (!approved) {
      return;
    }

    delete picked[String(item.host_id)];
    delete refusals[String(item.host_id)];

    return turnPage();
  }

  // The first page is fetched before the panel goes up, so that a refusal is
  // answered with the line above the screen rather than with an empty panel.
  await drawPage();

  drawn = { number: page.number, size: page.size };

  const panel = openModal({
    name: "host-keys",
    title: t("status.host-keys.title"),
    body: [
      element("p", t("status.host-keys.text")),
      problem,
      list,
      chosen
    ],
    buttons: [
      {
        label: t("status.host-keys-approve.button"),
        name: "approve",
        variant: "primary",
        press: function (button, close) {
          return approvePickedHostKeys(pickedList(), picked, refusals, button, close, problem,
            turnPage);
        }
      },
      { label: t("common.close.button"), name: "close" }
    ]
  });

  // openModal puts the panel on the page before it hands back the promise, so
  // the press at the bottom is there to be set from the list that was drawn
  // before it. The draw that ran above could not reach it.
  sayChosen();

  await panel;

  // The screen behind is drawn again whichever way the panel went, and not only
  // after the press at the bottom. The line over the table is a count of what
  // is waiting, this panel is where that count is changed, and a press on a row
  // changes it as much as the press at the bottom does.
  return drawStatus();
}

// hostKeysPageBox is the tick that takes the whole page, and null for a page
// with nothing it may take.
//
// It takes the Hosts of this page that are waiting for a first approval and
// leaves out the ones that were presented a key other than the one they are
// trusted on. The first is a step of registering a Host, and an upgrade puts
// every Host through it at once, which is what a box over a page is for; the
// second is a server that was rebuilt or a connection that is not reaching the
// server at all, and that is an answer to give one Host at a time, with the two
// fingerprints in front of you.
function hostKeysPageBox(items, picked, redraw) {
  const first = items.filter(function (item) {
    return !item.mismatch;
  });

  if (first.length === 0) {
    return null;
  }

  const row = document.createElement("label");

  row.className = "host-keys-all";
  row.dataset.all = "host-keys";

  const box = document.createElement("input");

  box.type = "checkbox";
  box.dataset.field = "host-keys-all";
  box.checked = first.every(function (item) {
    return Object.prototype.hasOwnProperty.call(picked, String(item.host_id));
  });

  box.addEventListener("change", function () {
    for (const item of first) {
      if (box.checked) {
        picked[String(item.host_id)] = item;
      } else {
        delete picked[String(item.host_id)];
      }
    }

    redraw();
  });

  const text = document.createElement("span");

  text.className = "host-keys-all-text";
  text.appendChild(element("span", t("status.host-keys-all.label")));

  const said = element("small", t("status.host-keys-all.hint"));

  said.className = "host-keys-said";
  text.appendChild(said);

  row.appendChild(box);
  row.appendChild(text);

  return row;
}

// hostKeyRow is one Host of that list: the tick, which Host it is, which of the
// two states it is in, the fingerprints, and the press that answers this one
// Host on its own.
//
// The box and the name of the Host are a label together, so the name is part of
// the target: a checkbox on its own is the width of a character, which is the
// one thing a list ticked on a phone cannot be. The rest of the row is outside
// that label, because a button inside one is pressed by a press meant for the
// box.
function hostKeyRow(item, picked, refused, sayChosen, approve) {
  const state = item.mismatch ? "host_key_mismatch" : "host_key_unapproved";
  const row = document.createElement("div");

  row.className = "host-key-row " + (item.mismatch ? "bad" : "waiting");
  row.dataset.hostKey = String(item.host_id);

  const pick = document.createElement("label");

  pick.className = "host-key-pick";

  const box = document.createElement("input");

  box.type = "checkbox";
  box.dataset.field = "host-key-" + item.host_id;
  box.checked = Object.prototype.hasOwnProperty.call(picked, String(item.host_id));
  box.addEventListener("change", function () {
    if (box.checked) {
      picked[String(item.host_id)] = item;
    } else {
      delete picked[String(item.host_id)];
    }

    sayChosen();
  });

  const name = document.createElement("span");

  name.className = "host-key-name";
  name.appendChild(element("span", t("status.host-key.title",
    { id: item.host_id, ip: item.ip })));
  name.appendChild(statusBadge(state));

  pick.appendChild(box);
  pick.appendChild(name);
  row.appendChild(pick);

  // The fingerprints, in the same two boxes the single approval puts them in. A
  // Host that is trusted on another key carries both, because the question that
  // is being answered about it is the comparison of the two.
  row.appendChild(hostKeyFingerprints(
    typeof item.trusted_fingerprint === "string" ? item.trusted_fingerprint : "",
    typeof item.fingerprint === "string" ? item.fingerprint : ""
  ));

  if (refused !== undefined && refused !== "") {
    const said = element("p", refused);

    said.className = "notice error";
    said.dataset.problem = "host-key-" + item.host_id;
    row.appendChild(said);
  }

  const buttons = document.createElement("div");

  buttons.className = "buttons";
  buttons.appendChild(actionButton(t("status.host-key-approve.button"),
    "host-key-" + item.host_id, approve, item.mismatch ? "danger" : undefined));

  row.appendChild(buttons);

  return row;
}

// approvePickedHostKeys is the press at the bottom of the panel.
//
// Nothing is sent from here. What it does is put the confirmation up, and the
// confirmation is what sends: the ticks were made over several pages, so most
// of what is about to be approved is not on the screen, and this is the one
// place it can all be read before it goes.
async function approvePickedHostKeys(chosen, picked, refusals, button, close, problem, turnPage) {
  if (chosen.length === 0) {
    showPanelProblem(problem, t("status.host-keys-none.error"));

    return;
  }

  // The button is held down for as long as the confirmation is up, because the
  // panel underneath stays where it is and a second press would put a second
  // confirmation over the first.
  button.disabled = true;

  try {
    const answer = await confirmHostKeyApprovals(chosen);
    if (answer === null) {
      return;
    }

    const results = answer.hosts === null || answer.hosts === undefined ? [] : answer.hosts;

    for (const result of results) {
      const key = String(result.host_id);

      delete refusals[key];

      if (result.approved) {
        // Approved, so it is no longer waiting: the row goes with the next
        // draw of the list, and the tick goes with it.
        delete picked[key];

        continue;
      }

      // The refusal is said in the language of the page where the code is one
      // this screen knows, and in the English the answer carries where it is
      // not, which is what an older screen meets from a newer server.
      const said = refusalText(result);

      refusals[key] = said === null ? String(result.error === undefined ? "" : result.error) : said;
    }

    const approved = results.length - Object.keys(refusals).length;

    if (Object.keys(refusals).length === 0) {
      setToast(function () {
        return t(plural(approved, "status.host-keys-approved-one.notice",
          "status.host-keys-approved-many.notice"), { count: approved });
      });

      close("approved");

      return;
    }

    // Something was refused, so the panel stays up with the reasons on the
    // rows they belong to. The list is read again first, which is what takes
    // the Hosts that did go through out of it.
    await turnPage();

    showPanelProblem(problem, t("status.host-keys-refused.notice",
      { approved: approved, refused: Object.keys(refusals).length }));
  } finally {
    button.disabled = false;
  }
}

// confirmHostKeyApprovals is the step between the ticks and the request: every
// Host that is about to be approved, with the fingerprint that is about to be
// trusted, and the password of the account where one of them replaces a key
// that is already trusted.
//
// It is here because the ticks outlive the page they were made on. What is
// about to be sent is mostly not on the screen behind this, and a press that
// approved it would be a press over a list nobody read; this is where it is
// read, with the same fingerprints that were ticked.
//
// It hands back what the server answered, or null where the panel was closed
// without sending. A refusal of the whole request is shown inside it and leaves
// it up, because the password is typed here and a panel that closed on a wrong
// one would throw the list away with it.
async function confirmHostKeyApprovals(chosen) {
  const changed = chosen.filter(function (item) {
    return item.mismatch;
  });

  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "host-keys-confirm";
  problem.hidden = true;

  // The password of the account is asked for exactly where the server reads
  // one: where the list carries a Host whose trusted key would be replaced. It
  // is asked once for the whole list and not once per Host, because what it
  // answers is whether the person at the screen is the one who logged in.
  const password = changed.length === 0 ? null : hostKeyPasswordField();

  const list = document.createElement("div");

  list.className = "host-key-list";
  list.dataset.list = "host-keys-confirm";

  for (const item of chosen) {
    list.appendChild(hostKeyConfirmRow(item));
  }

  const body = [element("p", t("status.host-keys-confirm.text"))];

  if (changed.length > 0) {
    const warning = element("p", t("status.host-keys-confirm-changed.text"));

    warning.className = "notice error";
    warning.dataset.problem = "host-keys-changed";
    body.push(warning);
  }

  body.push(problem, list);

  if (password !== null) {
    body.push(password.row);
  }

  let answer = null;

  const outcome = await openModal({
    name: "host-keys-confirm",
    title: t("status.host-keys-confirm.title"),
    body: body,
    buttons: [
      {
        label: t("status.host-keys-approve.button"),
        name: "approve",
        // A list that replaces a trusted key is painted as what cannot be
        // taken back, for the reason the single approval is.
        variant: changed.length === 0 ? "primary" : "danger",
        press: function (node, close) {
          return sendHostKeyApprovals(chosen, password, node, close, problem, function (data) {
            answer = data;
          });
        }
      },
      { label: t("common.cancel.button"), name: "cancel" }
    ]
  });

  return outcome === "approved" ? answer : null;
}

// hostKeyConfirmRow is one Host of that list: which Host it is, which of the
// two states it is in, and the fingerprint that is about to be trusted.
//
// A Host whose trusted key would be replaced is marked as such rather than left
// to be picked out of the list by its badge. It is the one row of the list that
// is not a registration, and the password at the bottom is being typed because
// of it.
function hostKeyConfirmRow(item) {
  const state = item.mismatch ? "host_key_mismatch" : "host_key_unapproved";
  const row = document.createElement("div");

  row.className = "host-key-row " + (item.mismatch ? "bad" : "waiting");
  row.dataset.hostKey = String(item.host_id);

  const name = document.createElement("div");

  name.className = "host-key-name";
  name.appendChild(element("span", t("status.host-key.title",
    { id: item.host_id, ip: item.ip })));
  name.appendChild(statusBadge(state));

  row.appendChild(name);
  row.appendChild(hostKeyFingerprints(
    typeof item.trusted_fingerprint === "string" ? item.trusted_fingerprint : "",
    typeof item.fingerprint === "string" ? item.fingerprint : ""
  ));

  return row;
}

// sendHostKeyApprovals sends the list that was confirmed.
//
// Every Host goes with the fingerprint that was drawn for it. The server takes
// each one only while that is still the key waiting on that Host, so a press
// meant for the keys that were read cannot approve one an SSH server presented
// after they were; a Host that meets that is refused on its own and the rest of
// the list goes through.
async function sendHostKeyApprovals(chosen, password, button, close, problem, keep) {
  const body = {
    hosts: chosen.map(function (item) {
      return { host_id: item.host_id, fingerprint: item.fingerprint };
    })
  };

  if (password !== null) {
    // An empty box is answered here rather than by a round trip, the way a
    // form answers a value the server would refuse anyway.
    if (password.input.value === "") {
      password.input.classList.add("bad");
      showPanelProblem(problem, t("status.host-key-password.error"));

      return;
    }

    password.input.classList.remove("bad");
    body.password = password.input.value;
  }

  // The button is held down for the whole call, because the panel stays up
  // while it is in flight and a second press would send the same list again.
  button.disabled = true;
  problem.hidden = true;

  try {
    keep(await apiCall("POST", "/api/host-key", body));

    close("approved");
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

// openHostKeyPanel is where the key an SSH server presented is compared with
// the server and approved, one Host at a time.
//
// The Host is read again on the way in rather than taken from the row that
// was pressed. A row of the list carries what was waiting when the page was
// read, and the screen behind this is a refresh or two old either way: what is
// put in front of the operator has to be the key that is waiting now, because
// that is the key the approval names.
//
// It hands back whether the key was approved. The screen behind is not drawn
// from here: this is opened from a panel that stays up over it, and what the
// list in that panel does about a Host that has been answered for is the
// caller's to decide.
async function openHostKeyPanel(hostID) {
  const host = await apiCall("GET", "/api/host/" + hostID);
  const waiting = typeof host.pending_host_key_fingerprint === "string"
    ? host.pending_host_key_fingerprint
    : "";
  const trusted = typeof host.host_key_fingerprint === "string"
    ? host.host_key_fingerprint
    : "";

  if (waiting === "") {
    // Approved from another client in the meantime, or nothing has connected
    // since the last one was approved. There is nothing to compare, so the
    // panel says so rather than showing an empty box with an approve under
    // it.
    await openModal({
      name: "host-key-gone",
      title: t("status.host-key.title", { id: host.id, ip: host.ip }),
      body: [element("p", t("status.host-key-gone.text"))],
      buttons: [{ label: t("common.close.button"), name: "close" }]
    });

    return false;
  }

  // Why an approval was refused. It is shown inside the panel because the line
  // above the screen is behind the backdrop, where the operator who pressed
  // the button cannot read it.
  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "host-key";
  problem.hidden = true;

  // The password of the account is asked for exactly where the server reads
  // one: on an approval that replaces a key that is already trusted. A Host
  // that carries none has no trust to overturn, and a box there would be a
  // password on the registration of every Host.
  const password = trusted === "" ? null : hostKeyPasswordField();

  const body = [
    element("p", trusted === ""
      ? t("status.host-key-first.text")
      : t("status.host-key-changed.text")),
    problem,
    hostKeyFingerprints(trusted, waiting)
  ];

  if (password !== null) {
    body.push(password.row);
  }

  const outcome = await openModal({
    name: "host-key",
    title: t("status.host-key.title", { id: host.id, ip: host.ip }),
    body: body,
    buttons: [
      {
        label: t("status.host-key-approve.button"),
        name: "approve",
        // An approval that replaces a trusted key is painted as what cannot
        // be taken back, because the key it drops is the one thing that would
        // have caught a server answering in place of this one.
        variant: trusted === "" ? "primary" : "danger",
        press: function (button, close) {
          return approveHostKey(host, waiting, password, button, close, problem);
        }
      },
      { label: t("common.close.button"), name: "close" }
    ]
  });

  return outcome === "approved";
}

// hostKeyFingerprints is the key that is waiting, and the key the Host is
// trusted on where it carries one, side by side.
//
// The two are drawn as two boxes of the same width rather than as a sentence
// each, because what is done with them is a comparison: the same face, the
// same width and the same wrapping put the character that differs of one
// under the character of the other.
function hostKeyFingerprints(trusted, waiting) {
  const pair = document.createElement("div");

  pair.className = "host-key-fingerprints";

  if (trusted !== "") {
    pair.appendChild(hostKeyFingerprint("trusted", t("status.host-key-trusted.label"), trusted));
  }

  pair.appendChild(hostKeyFingerprint("presented", t("status.host-key-presented.label"), waiting));

  return pair;
}

// hostKeyFingerprint is one of those boxes: which key it is, and the
// fingerprint itself in the face every fingerprint on these screens is in.
function hostKeyFingerprint(name, label, fingerprint) {
  const box = document.createElement("div");

  box.className = "host-key-fingerprint";
  box.dataset.fingerprint = name;
  box.appendChild(element("small", label));
  box.appendChild(fingerprintValue(fingerprint));

  return box;
}

// accountPasswordField is the box the password of the account is typed into
// inside a panel.
//
// It is built here rather than with buildForm, because what sends it is a
// button of the panel: a form inside a panel would send itself on Enter, and
// what a form sends is read by the form and not by the press that the panel
// settles on.
//
// The id is handed in so that two panels of the same screen cannot both call
// their box the same thing, which is what the label points at.
function accountPasswordField(id, label, hint) {
  const row = document.createElement("div");
  const name = element("label", label);
  const input = document.createElement("input");

  row.className = "field";
  name.htmlFor = id;

  input.type = "password";
  input.id = id;
  input.name = "password";
  input.dataset.field = "password";

  row.appendChild(name);
  row.appendChild(input);
  row.appendChild(element("small", hint));

  return { row: row, input: input };
}

// hostKeyPasswordField is that box on the panels of the Status screen.
function hostKeyPasswordField() {
  return accountPasswordField("host-key-password", t("status.host-key-password.label"),
    t("status.host-key-password.hint"));
}

// approveHostKey sends the approval of the key that was drawn.
//
// The fingerprint goes back with it. The server takes the approval only while
// that is still the key waiting on the Host, so a press meant for the key on
// the screen cannot approve one the SSH server presented after the screen was
// drawn; that refusal is shown in the panel, where the fingerprints the
// operator was comparing are still in front of them.
async function approveHostKey(host, waiting, password, button, close, problem) {
  const body = { fingerprint: waiting };

  if (password !== null) {
    // An empty box is answered here rather than by a round trip, the way a
    // form answers a value the server would refuse anyway.
    if (password.input.value === "") {
      password.input.classList.add("bad");
      showPanelProblem(problem, t("status.host-key-password.error"));

      return;
    }

    password.input.classList.remove("bad");
    body.password = password.input.value;
  }

  // The button is held down for the whole call, because the panel stays up
  // while it is in flight and a second press would send the same approval
  // again.
  button.disabled = true;
  problem.hidden = true;

  try {
    await apiCall("POST", "/api/host/" + host.id + "/host-key", body);

    setToast(function () {
      return t("status.host-key-approved.notice", { id: host.id, fingerprint: waiting });
    });

    close("approved");
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

// listNumbers is how many page numbers stand on the controls at once, and
// listNumbersNarrow is how many are left where there is no width for five.
//
// The count is picked here and not by a rule in the stylesheet because it is
// not only what is drawn. The two buttons that push the run of numbers along
// move it by exactly this many and go dead by where its ends fall, so numbers
// taken off the row by the stylesheet would leave both of them working on a
// run of five over a row that shows three.
const listNumbers = 5;
const listNumbersNarrow = 3;

// listNarrow is the width the run drops to three at. It is the width the rest
// of the screens turn over at, written in the same units, so that the controls
// narrow along with the page around them and not at a width of their own.
const listNarrow = "(max-width: 34rem)";

// The two buttons that push the run of numbers along, as characters rather
// than as words. SINGLE LEFT-POINTING ANGLE QUOTATION MARK is mirrored by the
// renderer on a page that reads right to left, so one pair points the way of
// whichever language it is drawn in; thirteen written-out arrows would each
// have to be pointed by hand. What they mean is said to a reader by their
// label, the way the theme switch says what it is.
const listEarlierNumbers = "‹‹";
const listLaterNumbers = "››";

// listNumberSpan is how many numbers this row has room for.
function listNumberSpan() {
  if (typeof window.matchMedia !== "function") {
    return listNumbers;
  }

  return window.matchMedia(listNarrow).matches ? listNumbersNarrow : listNumbers;
}

// listNumberFirst is the leftmost number on the row.
//
// The run is a block of the list rather than a window centred on the page: at
// five to a row pages one to five are one block and six to ten the next, so
// stepping from eight to nine leaves the numbers where they were instead of
// sliding them one place under the finger that pressed Next.
//
// page.window is a run that was pushed along by hand, and it is held against
// the page it was pushed from. Pushing is for reaching a page that is nowhere
// near this one, so it has to outlive the draw that comes five seconds later;
// the moment the page itself changes, though, a run pushed off somewhere else
// would no longer hold the page being read, so it is let go of and the block
// of the new page is drawn. It is kept on the page rather than beside it
// because the two panels carry a page of their own, and a run that outlived
// the panel it was pushed in would be waiting in the next one.
function listNumberFirst(page, span, last) {
  const pushed = page.window;

  if (pushed !== undefined && pushed !== null && pushed.page === page.number &&
    pushed.first >= 1 && pushed.first <= last) {
    return pushed.first;
  }

  return Math.floor((page.number - 1) / span) * span + 1;
}

// pageControls is the row above a table: the size the list is read in, the way
// to the page on either side, the numbers of the pages around this one, and
// where in the list this page is.
//
// Nothing is drawn while there is nothing the row could do. A list shorter than
// the smallest size is one page at every size, so both buttons are dead and the
// list of sizes changes nothing, and three rows would carry a row of controls
// that only says there are three rows. It appears as soon as one of the sizes
// would split the list, which includes the case where the size in use does not:
// that is the state a screen is left in by choosing a hundred, and controls
// that took themselves away there would leave no way back to ten.
//
// name is what the controls are named by, so that the row over a list and the
// row inside a panel opened over it are not two buttons of the same name in
// one document. Each works either way, since each holds its own draw, but
// anything that finds a button by its name would find the wrong one.
function pageControls(name, page, total, draw) {
  if (total <= listSizes[0]) {
    return null;
  }

  const row = document.createElement("div");

  row.className = "page-controls";

  // The same list, built by the same function as the ones above the log, so
  // that the two rows of controls are one thing to learn rather than two.
  row.appendChild(logSelect(name + "-page-size", t("list.page-size.label"), listSizes, page.size,
    function (value) {
      page.size = Number(value);
      // The rows move under the numbering when the size changes, so the page
      // that was being read is no longer a place. The first page is the one
      // page that means the same at every size.
      page.number = 1;
      // And a run pushed along by hand was pushed over a list cut into other
      // pages. It is let go of here rather than by the page changing, because
      // a size chosen while the first page is up changes nothing for that
      // test to see.
      page.window = null;

      return draw();
    }));

  const last = lastPageOf(total, page.size);

  // The buttons are held in a run of their own. The row wraps where there is
  // no width for all of it, and what it may break between is the size, the
  // buttons and the line saying where this is; a break through the middle of
  // the numbers would leave one page on the line under the page before it.
  const turn = document.createElement("div");

  turn.className = "page-turn";

  const previous = actionButton(t("list.previous.button"), name + "-page-previous", function () {
    page.number = page.number - 1;

    return draw();
  });

  previous.disabled = page.number <= 1;
  turn.appendChild(previous);

  const span = listNumberSpan();
  const first = listNumberFirst(page, span, last);
  const until = Math.min(first + span - 1, last);

  // The two that push the run along leave the page alone: they are how a page
  // far from this one is reached, and a press that also turned the page would
  // make the numbers somewhere to go rather than somewhere to look.
  const earlier = actionButton(listEarlierNumbers, name + "-page-numbers-earlier", function () {
    page.window = { first: Math.max(1, first - span), page: page.number };

    return draw();
  }, "page-shift");

  earlier.setAttribute("aria-label", t("list.numbers-earlier.aria"));
  earlier.disabled = first <= 1;
  turn.appendChild(earlier);

  for (let number = first; number <= until; number += 1) {
    const here = number === page.number;
    const pick = actionButton(String(number), name + "-page-number-" + number, function () {
      page.number = number;

      return draw();
    }, here ? "page-number page-number-here" : "page-number");

    // The number carries the digit alone, which says nothing on its own to a
    // reader going button by button.
    pick.setAttribute("aria-label", t("list.page.aria", { page: number }));

    // The page being read is on the row to be found and not to be pressed: a
    // press on it would fetch the page that is already on the screen.
    pick.disabled = here;

    if (here) {
      pick.setAttribute("aria-current", "page");
    }

    turn.appendChild(pick);
  }

  const later = actionButton(listLaterNumbers, name + "-page-numbers-later", function () {
    page.window = { first: Math.min(last, first + span), page: page.number };

    return draw();
  }, "page-shift");

  later.setAttribute("aria-label", t("list.numbers-later.aria"));
  later.disabled = until >= last;
  turn.appendChild(later);

  const next = actionButton(t("list.next.button"), name + "-page-next", function () {
    page.number = page.number + 1;

    return draw();
  });

  next.disabled = page.number >= last;
  turn.appendChild(next);

  row.appendChild(turn);

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

  // How many numbers there is room for is read once, here, at the moment of
  // the draw, so a window dragged narrower would keep five of them until
  // something else drew the screen again. This is what draws it again, and it
  // goes with the row it was made for: the first crossing takes it off, and a
  // row that is no longer on the page is one whose draw belongs to a screen
  // that has been left, so the crossing that took it off is the whole of it.
  if (typeof window.matchMedia === "function") {
    const width = window.matchMedia(listNarrow);
    const crossed = function () {
      width.removeEventListener("change", crossed);

      if (row.isConnected) {
        run(draw);
      }
    };

    width.addEventListener("change", crossed);
  }

  return row;
}

function enterHosts() {
  editingHostID = null;

  return drawHosts();
}

async function drawHosts() {
  const page = listPages.hosts;

  // Taken before the fetch, so that a press that is made while this one is in
  // the air leaves its own reasons for the draw it sets off and not for this
  // one. Every draw empties it, which is what keeps the reasons of one press
  // off the screen of the next.
  const refusals = pickedFlipRefusals;

  pickedFlipRefusals = [];

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

  // And the same for what is ticked, for the same reason and one more: the
  // rows of this page are the rows a tick may be held for at all.
  keepPicksOnPage("hosts", hosts);

  const nodes = [editing === undefined ? hostCreateForm() : hostEditForm(editing)];

  if (hosts.length === 0) {
    nodes.push(statusLine(t("hosts.none.empty"), "empty"));
  } else {
    const controls = pageControls("hosts", page, total, drawHosts);
    if (controls !== null) {
      nodes.push(controls);
    }

    const picks = listPickColumn("hosts", hosts, function (id) {
      return t("hosts.pick-row.aria", { id: id });
    });

    const table = buildTable(
      [t("hosts.id.column"), t("hosts.ip.column"), t("hosts.port.column"),
        t("hosts.user.column"), t("hosts.description.column"),
        t("hosts.enabled.column"), t("hosts.socks.column"), t("hosts.updated.column"), ""],
      hosts.map(function (host) {
        const row = { cells: hostRow(host), pick: picks.box(host.id) };
        const failure = host.socks_enabled && typeof host.socks_last_error === "string"
          ? host.socks_last_error : "";

        if (failure !== "") {
          row.under = element("span", failure);
          row.under.className = "last-error";
        }

        return row;
      }),
      [0, 2],
      picks.head
    );
    table.dataset.list = "hosts";

    // The press that acts on the ticks goes under the controls that turn the
    // page and over the rows it acts on, which are the rows of this page: a
    // tick is held for nothing else.
    const bar = deletePickedBar({
      name: "hosts",
      items: hosts,
      table: table,
      label: t("hosts.delete-picked.button"),
      title: t("hosts.delete-picked.title"),
      text: t("hosts.delete-picked.text"),
      describe: function (host) {
        return t("hosts.picked-row.text", { id: host.id, ip: host.ip });
      },
      path: function (host) {
        return "/api/host/" + host.id;
      },
      said: function (count) {
        return t(plural(count, "hosts.deleted-picked-one.notice",
          "hosts.deleted-picked-many.notice"), { count: count });
      },
      partly: function (deleted, failed) {
        return t("hosts.deleted-picked-some.notice", { deleted: deleted, failed: failed });
      },
      draw: drawHosts
    });

    // The two presses that turn what is ticked on and off, over the same ticks
    // the delete is over. They are the Host list's alone: there is no flag on
    // a service port to turn.
    //
    // They go in front of the delete and not after it, which is why they are
    // put in rather than appended. The delete cannot be taken back and these
    // two can, so the delete is not the press a hand reaching along the row
    // lands on first.
    const flips = document.createDocumentFragment();

    flips.appendChild(ticksWakeThePress("hosts", table,
      actionButton(t("hosts.enable-picked.button"), "hosts-enable-picked",
        function () {
          return flipPickedHosts(hosts, true);
        })));
    flips.appendChild(ticksWakeThePress("hosts", table,
      actionButton(t("hosts.disable-picked.button"), "hosts-disable-picked",
        function () {
          return flipPickedHosts(hosts, false);
        })));

    bar.insertBefore(flips, bar.firstChild);

    nodes.push(bar);

    // What the last flip could not change goes between the presses and the
    // rows, which is where the ticks that were sent are still on the screen.
    if (refusals.length > 0) {
      nodes.push(flipRefusalList(refusals));
    }

    nodes.push(table);
  }

  render(t("hosts.screen.title"), nodes);
}

// flipRefusalList names the Hosts a flip left as they were, each with what the
// server said about that one.
//
// A row is named by the identifier and the address the table names it by,
// which is what the rows of a batch delete are named by, and it is painted as
// what did not happen for the reason those are: the list is only ever the rows
// that were refused.
function flipRefusalList(refusals) {
  const list = document.createElement("div");

  list.className = "picked-list";
  list.dataset.list = "hosts-flip-picked";

  for (const refusal of refusals) {
    const row = document.createElement("div");
    const said = element("p", refusal.reason);

    row.className = "picked-row bad";
    row.dataset.picked = String(refusal.host.id);
    said.className = "picked-said";

    row.appendChild(element("span", t("hosts.picked-row.text",
      { id: refusal.host.id, ip: refusal.host.ip })));
    row.appendChild(said);

    list.appendChild(row);
  }

  return list;
}

// flipPickedHosts turns every ticked Host on, or every ticked Host off.
//
// There is no confirmation over it, unlike the batch delete beside it. What it
// does is undone by the other of the two presses, and a step in front of
// something that can be taken back is a step that is read once and pressed
// through from then on.
//
// One request per Host and not one for the batch, for the reason the batch
// delete sends one at a time: the database runs on a single connection, and a
// request that wrote a hundred rows in one transaction would hold it for the
// whole of them. A refusal stops that Host and nothing else, since what was
// asked for was the rest of the list as much as that row.
async function flipPickedHosts(hosts, on) {
  const wanted = pickedIDs("hosts");
  const chosen = hosts.filter(function (host) {
    return wanted.indexOf(host.id) !== -1;
  });

  // Nothing is ticked, which is the state the press is dead in. It is read
  // again here rather than trusted to the button, which was drawn with the
  // page while the ticks have been changing since.
  if (chosen.length === 0) {
    return;
  }

  const refusals = [];
  let flipped = 0;
  let already = 0;

  for (const host of chosen) {
    // A Host that is already the way the press asks for is counted and not
    // sent. The request would not be free: the server saves the row it read
    // and wakes the reconcile pass whatever the row was, so a Host that is
    // already enabled would come back with a new time in the Updated column
    // and nothing else changed, and that column is what says when a Host was
    // last touched. It would also take its turn on the one database
    // connection, in front of the Hosts the press is actually for.
    if (host.enabled === on) {
      already += 1;

      continue;
    }

    try {
      await apiCall("PUT", "/api/host/" + host.id, { enabled: on });
    } catch (error) {
      if (error instanceof Redirected) {
        // The session ended and the page is on its way to the login. What is
        // left of the list is not sent after it.
        throw error;
      }

      refusals.push({ host: host, reason: error.message });

      continue;
    }

    flipped += 1;
  }

  // The ticks stay on. The rows are all still there, unlike the rows of a
  // batch delete, and the next thing done to them is usually done to the same
  // ones: a flip that was refused in part is pressed again, and a batch that
  // went through is often the batch that is then deleted.
  if (refusals.length > 0) {
    pickedFlipRefusals = refusals;

    setFailure(function () {
      return on
        ? t("hosts.enabled-picked-some.notice",
          { flipped: flipped, already: already, refused: refusals.length })
        : t("hosts.disabled-picked-some.notice",
          { flipped: flipped, already: already, refused: refusals.length });
    });
  } else if (already > 0) {
    setToast(function () {
      return on
        ? t("hosts.enabled-picked-same.notice", { flipped: flipped, already: already })
        : t("hosts.disabled-picked-same.notice", { flipped: flipped, already: already });
    });
  } else {
    setToast(function () {
      return t(on
        ? plural(flipped, "hosts.enabled-picked-one.notice", "hosts.enabled-picked-many.notice")
        : plural(flipped, "hosts.disabled-picked-one.notice", "hosts.disabled-picked-many.notice"),
      { count: flipped });
    });
  }

  return drawHosts();
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
  buttons.appendChild(actionButton(t("hosts.local-forwards.button"), "host-local-forwards-" + host.id, function () {
    return openHostLocalForwards(host);
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
    socksCell(host),
    timeCell(host.updated_at),
    buttons
  ];
}

function socksCell(host) {
  if (!host.socks_enabled || host.socks_status === "off") {
    return "-";
  }

  const cell = document.createElement("span");

  cell.className = "socks-cell";
  cell.dataset.socks = String(host.id);
  cell.appendChild(document.createTextNode(String(host.socks_port) + " "));
  cell.appendChild(statusBadge(host.socks_status));

  return cell;
}

// deletePickedBar is the row over a list that carries the press acting on what
// is ticked: the one that takes every ticked row away.
//
// The press is drawn whether or not anything is ticked, and is dead while
// nothing is. A button that appeared with the first tick would move the table
// under the hand that is ticking it, and one that is there and dead says what
// the ticks are for before any of them is made.
//
// The rest of what one of these lists differs in is handed in by the screen
// that draws it: the words, the path a row is deleted at, and how a row is
// named in front of the operator. What is here is what the two lists do the
// same way, which is everything about the sending.
//
// The row that comes back is the row and not the button, so a screen with a
// second press over the ticks puts it into the row beside this one. Both lists
// have one and neither is the other's: the service port list assigns what is
// ticked to Hosts, and the Host list turns what is ticked on and off.
//
// Where the presses do not fit on one line they fold into one menu button,
// the ones put in after this included, rather than wrap onto a second line.
function deletePickedBar(spec) {
  const row = document.createElement("div");

  row.className = "list-actions";

  row.appendChild(ticksWakeThePress(spec.name, spec.table,
    actionButton(spec.label, spec.name + "-delete-picked", function () {
      return deletePicked(spec);
    }, "danger")));

  return foldingBar(row);
}

// ticksWakeThePress is the rule every press over the ticks of a list is under:
// it is dead while nothing on the page is ticked.
//
// What is ticked changes without a draw, since a tick writes listPicks and
// fetches nothing, so the change of a box is listened for on the table the
// ticks are in: every box of the page is under it, the head of the table
// included, and the press goes dead again the moment the last tick is
// released.
function ticksWakeThePress(name, table, button) {
  const settle = function () {
    button.disabled = pickedIDs(name).length === 0;
  };

  settle();
  table.addEventListener("change", settle);

  return button;
}

// deletePicked is the press: the ticked rows of the page, read out of the
// answer the page was drawn from, put in front of the operator once more
// before any of them is sent.
//
// The list is drawn again however the confirmation ended. A press that sent
// nothing leaves the list as it was, and one that deleted part of it leaves
// rows of this page gone and rows of the pages after it moved up into their
// places; neither is what is on the screen behind the panel.
async function deletePicked(spec) {
  const wanted = pickedIDs(spec.name);
  const chosen = spec.items.filter(function (item) {
    return wanted.indexOf(item.id) !== -1;
  });

  // Nothing is ticked, which is the state the press is dead in. It is read
  // again here rather than trusted to the button, which was drawn with the
  // page while the ticks have been changing since.
  if (chosen.length === 0) {
    return;
  }

  await confirmPickedDeletes(spec, chosen);

  return spec.draw();
}

// confirmPickedDeletes is the step between the ticks and the requests.
//
// It is here because what is about to go cannot be brought back from this
// screen or from any other. The ticks of a page of a hundred are not something
// a glance at the table confirms, and a row ticked by a press on the head of
// the table was never read at all; this is where every one of them is named
// before the first request goes out.
//
// It is also where the end of the run is reported. A batch is a run of
// requests and not one, so it ends as a count that went and a count that did
// not: where everything went, the panel closes with the count on the line over
// the screen, and where something did not, it stays up holding those rows
// alone with what each of them met. The press is then a second run over what
// is left, which is what a row that was refused for a reason that has since
// passed needs.
async function confirmPickedDeletes(spec, chosen) {
  // What the next press would send. It starts as everything that was ticked
  // and becomes the rows that were refused, so a run after a partial one goes
  // over those and not over the rows that are already gone.
  let remaining = chosen;

  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = spec.name + "-delete-picked";
  problem.hidden = true;

  const list = document.createElement("div");

  list.className = "picked-list";
  list.dataset.list = spec.name + "-delete-picked";

  const fill = function (rows) {
    list.textContent = "";

    for (const row of rows) {
      list.appendChild(pickedDeleteRow(spec, row.item, row.reason));
    }
  };

  fill(chosen.map(function (item) {
    return { item: item, reason: null };
  }));

  await openModal({
    name: spec.name + "-delete-picked",
    title: spec.title,
    body: [element("p", spec.text), problem, list],
    buttons: [
      {
        label: t("common.delete.button"),
        name: "delete",
        // Painted as what cannot be taken back, the way the delete on a single
        // row is.
        variant: "danger",
        press: function (node, close) {
          return sendPickedDeletes(spec, remaining, node, close, problem, function (failures) {
            remaining = failures.map(function (failure) {
              return failure.item;
            });

            fill(failures);
          });
        }
      },
      { label: t("common.cancel.button"), name: "cancel" }
    ]
  });
}

// pickedDeleteRow is one row of that list: what the row is, by the same
// identifier and address the table names it by, and where it was refused, what
// the server said about it.
function pickedDeleteRow(spec, item, reason) {
  const row = document.createElement("div");
  const refused = reason !== null && reason !== undefined;

  row.className = refused ? "picked-row bad" : "picked-row";
  row.dataset.picked = String(item.id);
  row.appendChild(element("span", spec.describe(item)));

  if (refused) {
    const said = element("p", reason);

    said.className = "picked-said";
    row.appendChild(said);
  }

  return row;
}

// sendPickedDeletes sends the list that was confirmed, one row at a time.
//
// One request per row and not one for the batch: the database runs on a single
// connection, so a request that deleted a hundred rows in one transaction
// would hold it for the whole of them and everything else, the reconcile pass
// included, would wait behind it. A row at a time puts the connection down
// between rows.
//
// A refusal stops that row and nothing else. The rows are separate deletions
// and a row that is already gone, or that another screen is holding, says
// nothing about the next one; what the operator asked for was the rest of the
// list as much as that row. The counts and the reasons are what the run is
// answered with, which is the same as what the batch of host key approvals
// hands back.
async function sendPickedDeletes(spec, chosen, button, close, problem, keep) {
  // Held down for the whole run rather than for one request, because the panel
  // stays up until the last of them is answered and a second press would send
  // the list again from the top.
  button.disabled = true;
  problem.hidden = true;

  const failures = [];
  let deleted = 0;

  try {
    for (const item of chosen) {
      try {
        await apiCall("DELETE", spec.path(item));
      } catch (error) {
        if (error instanceof Redirected) {
          // The session ended and the page is on its way to the login. What is
          // left of the list is not sent after it.
          close(null);

          throw error;
        }

        failures.push({ item: item, reason: error.message });

        continue;
      }

      deleted += 1;

      // The row is gone, so the tick for it goes here and not with the draw
      // that follows. The draw drops it too, since it is no longer in the
      // answer, but between now and then the tick would be a tick held for a
      // row that does not exist.
      delete listPicks[spec.name][String(item.id)];
    }
  } finally {
    button.disabled = false;
  }

  if (failures.length === 0) {
    setToast(function () {
      return spec.said(deleted);
    });

    close("deleted");

    return;
  }

  // Something was refused, so the panel stays up with those rows and what each
  // of them met. The rows that went are taken out of it: they are gone, and a
  // list that still named them would be read as a list of what is about to be
  // deleted, which is what it is about to become again.
  keep(failures);

  showPanelProblem(problem, spec.partly(deleted, failures.length));
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

function portField(name, label, value, advise) {
  const field = {
    name: name,
    label: label,
    value: value,
    hint: t("form.port-example.hint"),
    inputMode: "numeric",
    filter: portCharacters,
    check: checkPort
  };

  if (advise !== undefined) {
    field.advise = advise;
  }

  return field;
}

// privilegedPortAdvice warns about a port the Host may not let this program
// open, without refusing it.
//
// It is a warning and not a check because whether the port can be opened is a
// fact about the far machine, not about the value: the account this Host is
// registered with may be root, the Host may be Windows, where the rule does not
// exist at all, and a Linux Host may have been told to let ordinary accounts
// open lower ports. Refusing here would be deciding all of that from a screen
// that cannot see any of it.
//
// It says nothing about the service port, which is the port this program
// connects out to rather than the one the Host is asked to open.
function privilegedPortAdvice(value) {
  const port = Number(String(value).trim());

  if (!Number.isInteger(port) || port < 1 || port >= 1024) {
    return "";
  }

  return t("service-ports.local-port-privileged.hint");
}

// bindScopeOptions are the two entries every list that picks a scope offers.
// The wildcard is first because it is what an assignment that says nothing
// means: a list that opened on the other one would send a narrower reach than
// the request it stands in used to make.
//
// There is no third entry and there is nothing to type. An address that belongs
// to one interface of one machine is a fact about that machine, and nothing
// here can ask a Host what its interfaces are: a typed address that is wrong is
// refused nowhere, it is a forward that quietly fails.
//
// The words are read as the list is built rather than once at the top of this
// file, because the catalog is loaded after the file is and the language is
// changed while the page is up.
function bindScopeOptions() {
  return [
    { value: bindScopeWildcard, text: t("form.bind-scope-wildcard.option") },
    { value: bindScopeLoopback, text: t("form.bind-scope-loopback.option") }
  ];
}

// bindScopeStored is what a form or a row is opened on. Anything that is not
// the loopback is the wildcard, which is what the empty value of the column
// means and what a row stored before there was a column carries.
function bindScopeStored(value) {
  return value === bindScopeLoopback ? bindScopeLoopback : bindScopeWildcard;
}

// bindScopeField is the list that picks how far the forwarded ports reach. It
// is built here rather than written out at each of the places it appears, for
// the reason ipField is: the two add forms and the assignment panel have to
// offer the same two answers under the same words.
//
// shownWhen, where a caller passes one, is the tick that decides whether the
// list is asked for at all. Both add forms make assignments only while a box is
// ticked, and a scope with no assignment to sit on is a question about nothing.
function bindScopeField(value, shownWhen) {
  const field = {
    name: "bind_scope",
    label: t("form.bind-scope.label"),
    value: bindScopeStored(value),
    options: bindScopeOptions(),
    note: t("form.bind-scope.hint"),
    advise: bindScopeAdvice
  };

  if (shownWhen !== undefined) {
    field.shownWhen = shownWhen;
  }

  return field;
}

// bindScopeAdvice says what the wildcard means, without refusing it.
//
// It is a warning for the reason privilegedPortAdvice is one: how far the port
// really reaches is a fact about the Host and about the SSH server on it, which
// this screen cannot see. An sshd with GatewayPorts off binds loopback whatever
// is asked for, and one with it on opens the port to everything that can reach
// that machine. Refusing the wildcard would be deciding that from here, and it
// is what every assignment stored so far is already running on.
function bindScopeAdvice(value) {
  if (value !== bindScopeWildcard) {
    return "";
  }

  return t("form.bind-scope-open.notice");
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
      },
      // The scope rides on that tick, and it is the scope of the assignments
      // this registration makes rather than of the Host: a Host holds no scope
      // at all. It is asked for here because the assignments are made here, and
      // what they are changed to afterwards is the panel behind the Service
      // ports button.
      bindScopeField(undefined, { field: "assign_all_service_ports", ticked: true })
    ].concat(socksFields({})),
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
      // No scope is asked for here. It belongs to the assignment and not to the
      // Host, so one answer on this form would be one answer for every service
      // port the Host carries, which is the thing the scope was moved off the
      // Host to stop. It is changed in the panel behind the Service ports
      // button, a row at a time or over everything that is ticked.
      { name: "description", label: t("hosts.description.label"), value: host.description },
      { name: "enabled", label: t("hosts.enabled.label"), type: "checkbox", value: host.enabled }
    ].concat(socksFields(host)),
    onSubmit: function (values) {
      return updateHost(host, values);
    },
    onCancel: function () {
      editingHostID = null;

      return drawHosts();
    }
  });
}

const socksDefaultPort = 1080;

function socksFields(host) {
  const shown = { field: "socks_enabled", ticked: true };
  const stored = typeof host.socks_port === "number" && host.socks_port > 0
    ? host.socks_port
    : socksDefaultPort;

  return [
    {
      name: "socks_enabled",
      label: t("hosts.socks-enabled.label"),
      type: "checkbox",
      value: host.socks_enabled === true,
      note: t("hosts.socks-enabled.hint")
    },
    Object.assign(portField("socks_port", t("hosts.socks-port.label"), stored),
      { shownWhen: shown }),
    {
      name: "socks_bind_scope",
      label: t("local-forwards.scope.label"),
      value: bindScopeStored(host.socks_bind_scope),
      options: localForwardScopeOptions(),
      shownWhen: shown
    },
    {
      name: "socks_allowed_sources",
      label: t("hosts.socks-sources.label"),
      value: typeof host.socks_allowed_sources === "string" ? host.socks_allowed_sources : "",
      note: t("hosts.socks-sources.hint"),
      shownWhen: shown
    }
  ];
}

async function createHost(values) {
  const body = {
    ip: values.ip.trim(),
    port: asNumber(values.port),
    user: values.user.trim(),
    description: values.description,
    assign_all_service_ports: values.assign_all_service_ports
  };

  // The scope is sent only where there are assignments for it to land on. The
  // API reads it while it makes them and ignores it otherwise, and a field that
  // decides nothing is better left out than sent from a row nobody saw.
  if (values.assign_all_service_ports) {
    body.bind_scope = values.bind_scope;
  }

  if (values.socks_enabled) {
    body.socks_enabled = true;
    body.socks_port = asNumber(values.socks_port);
    body.socks_bind_scope = values.socks_bind_scope;
    body.socks_allowed_sources = values.socks_allowed_sources.trim();
  }

  body.password = values.password;

  // The key boxes are sent only when they hold something, so that a host
  // registered with a password alone carries no empty key.
  const privateKey = values.private_key.trim();
  if (privateKey !== "") {
    body.private_key = privateKey;
    body.key_passphrase = values.key_passphrase;
  }

  await apiCall("POST", "/api/host", body);

  setToast(function () {
    return t("hosts.added.notice", { ip: body.ip });
  });

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

  body.socks_enabled = values.socks_enabled;
  if (values.socks_enabled) {
    body.socks_port = asNumber(values.socks_port);
    body.socks_bind_scope = values.socks_bind_scope;
    body.socks_allowed_sources = values.socks_allowed_sources.trim();
  }

  await apiCall("PUT", "/api/host/" + host.id, body);

  editingHostID = null;
  setToast(function () {
    return t("hosts.updated.notice", { id: host.id });
  });

  return drawHosts();
}

async function toggleHost(host) {
  await apiCall("PUT", "/api/host/" + host.id, { enabled: !host.enabled });

  const was = host.enabled;

  setToast(function () {
    return t(was ? "hosts.now-disabled.notice" : "hosts.now-enabled.notice", { id: host.id });
  });

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

  setToast(function () {
    return t("hosts.deleted.notice", { id: host.id });
  });

  return drawHosts();
}

// openHostServicePorts puts up the panel that says which service ports a Host
// carries, lets them be ticked, and says how far each of them reaches.
//
// The list is served a page at a time, so what was ticked is held in maps
// rather than read off the controls at the end. picks.served is what the server
// said about each service port on the pages that were read and picks.wanted
// holds the boxes the operator touched; picks.scopes and picks.scoped are the
// same pair for the scope. A control is drawn from what was touched where there
// is an entry for it and from what the server said otherwise, which is what
// keeps a tick made on the first page while the second one is being read and
// after coming back.
//
// What is sent is the difference between the two halves. The panel knows
// nothing of the pages it has not read, so a request carrying the whole set
// would name this page alone, and every assignment outside it would be deleted
// by a press that was meant to tick one box.
async function openHostServicePorts(host) {
  // The panel has a page of its own and does not touch listPages. It is opened
  // and closed while the list behind it stays where it is, and the two lists
  // are not the same length anyway.
  const page = { number: 1, size: listSizes[0] };

  // lists holds the scope list of every row on the page that is on the screen
  // now. It is what lets an apply over the ticked rows show up on the ones
  // being looked at, and it is emptied whenever the list is built again.
  const picks = { served: {}, wanted: {}, scopes: {}, scoped: {}, lists: {} };

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

  // What an apply over the ticked rows did. It is a separate line from the one
  // above because it is not a refusal, and it is inside the panel for the same
  // reason that one is: setNotice writes behind the backdrop.
  const said = element("p", "");

  said.className = "notice info";
  said.dataset.said = "host-service-ports";
  said.hidden = true;

  // The rows of the page that is on the screen, and how long the whole list is.
  // They are kept so that the tick which takes the whole page can draw the list
  // again without asking the server for a page it already has.
  let shown = [];
  let total = 0;

  function drawList() {
    // Only the list is built again. The panel around it is the one openModal
    // put up, and nothing here writes to #app, so a draw of the screen behind
    // the backdrop cannot take the panel down and this cannot draw over it.
    list.textContent = "";
    picks.lists = {};

    if (shown.length === 0) {
      list.appendChild(statusLine(t("service-ports.none.empty"), "empty"));

      return;
    }

    const controls = pageControls("host-service-ports", page, total, turnPage);
    if (controls !== null) {
      list.appendChild(controls);
    }

    // What the server said about this page is taken before anything is drawn,
    // because the tick in the head of the table is drawn from it: a row nobody
    // has touched is ticked where the Host carries it, and that answer has to
    // be in the maps before the head asks whether the page is ticked through.
    for (const item of shown) {
      picks.served[item.id] = Boolean(item.assigned);
      picks.scopes[item.id] = bindScopeStored(item.bind_scope);
    }

    const column = assignPickColumn(shown, picks);

    // The columns are the ones the service port list is read by, in the order
    // it reads them, so a row is recognised here by what it is called there:
    // the identifier, where the service is, which port it is opened on and
    // what somebody wrote it down as. The address is one column rather than
    // two because this table is read inside a panel, where every column costs
    // width the sentence above it needs. The reach is last because it is the
    // one column that is not read but answered.
    list.appendChild(buildTable(
      [t("service-ports.id.column"), t("hosts.assign-service.column"),
        t("service-ports.local-port.column"), t("service-ports.description.column"),
        t("hosts.assign-scope.column")],
      shown.map(function (item) {
        return { cells: servicePortAssignRow(item, picks), pick: column.box(item) };
      }),
      [0, 2],
      column.head
    ));
  }

  async function drawPage() {
    const answer = await apiCall("GET",
      "/api/host/" + host.id + "/service-port?" + pageQuery(page));

    takeListPage(page, answer);

    shown = answer === null || answer.items === null || answer.items === undefined
      ? []
      : answer.items;
    total = answer === null || typeof answer.total !== "number" ? shown.length : answer.total;

    drawList();
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
      element("p", t("hosts.assign-scope.text")),
      problem,
      said,
      scopeToTheTicked(picks, said),
      list
    ],
    buttons: [
      {
        label: t("common.save.button"),
        name: "save",
        variant: "primary",
        press: function (button, close) {
          return saveHostServicePorts(host, picks, button, close, problem);
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

// scopeToTheTicked is the row above the list that sets one scope on everything
// that is ticked.
//
// It reaches every row the panel has read and not the page on the screen alone,
// which is the same span the save works over: a tick made on the first page is
// still a tick while the second one is being read, and an apply that skipped it
// would be applying to a different set than the one that is about to be sent.
//
// It touches nothing that is not ticked. A row the operator left alone has an
// assignment somebody chose the reach of, or no assignment at all, and neither
// is this press's to change. What it writes is the map the save reads, so the
// rows it did not name are not in the request either.
function scopeToTheTicked(picks, said) {
  const row = document.createElement("div");

  row.className = "assign-bulk";

  const label = element("span", t("hosts.assign-scope.label"));

  label.className = "assign-bulk-label";

  const chosen = listControl({ options: bindScopeOptions(), value: bindScopeWildcard });

  chosen.className = "assign-bulk-scope";
  chosen.dataset.field = "assign-scope";
  chosen.setAttribute("aria-label", t("hosts.assign-scope.label"));

  row.appendChild(label);
  row.appendChild(chosen);
  row.appendChild(actionButton(t("hosts.assign-scope.button"), "assign-scope-apply", function () {
    let touched = 0;

    for (const id of Object.keys(picks.served)) {
      const ticked = id in picks.wanted ? picks.wanted[id] : picks.served[id];
      if (!ticked) {
        continue;
      }

      picks.scoped[id] = chosen.value;
      touched += 1;

      if (id in picks.lists) {
        picks.lists[id].value = chosen.value;
      }
    }

    sayInPanel(said, touched === 0
      ? t("hosts.assign-scope-none.notice")
      : t(plural(touched, "hosts.assign-scope-applied-one.notice",
        "hosts.assign-scope-applied-many.notice"), { count: touched }));
  }));

  return row;
}

// assignPickColumn makes the column of ticks for that panel: the one in the
// head of the table, which takes and releases the rows of the page on the
// screen, and the one of each row.
//
// It is the pair listPickColumn makes for the two lists, over the maps this
// panel holds instead of over listPicks, and the head follows the same rule
// there: a page whose rows are all ticked shows it ticked, and one row cleared
// clears it. What differs is what a tick is. On a list it is held for a press
// that is about to be made, so turning the page drops it; here it is the
// assignment itself, held for every page the panel has read, so nothing in
// here writes over a row that is not on the page underneath the head.
//
// Clearing the head is the press that takes assignments away, and there is no
// confirmation over it and nothing said back. Nothing is stored until Save,
// which is what the line at the top of the panel says, and the ticks it
// cleared are on the screen under it; closing the panel drops the lot, which
// is an escape from a misplaced press that a sentence would not add to.
function assignPickColumn(items, picks) {
  const boxes = [];
  const head = document.createElement("input");

  // What a row is ticked as now: what the operator touched where there is an
  // entry for it, and what the server said about the row otherwise.
  function ticked(id) {
    return id in picks.wanted ? picks.wanted[id] : picks.served[id];
  }

  // The scope list of a row is disabled while its box is clear: a service port
  // this Host does not carry has no forwarded port to open anywhere, so there
  // is nothing for the answer to be about. It is looked for when the press
  // happens rather than held here, because the head is built before the rows
  // it reaches are.
  function hold(id, on) {
    picks.wanted[id] = on;

    if (id in picks.lists) {
      picks.lists[id].disabled = !on;
    }
  }

  head.type = "checkbox";
  head.dataset.field = "assign-pick-all";
  // The head carries a name to be read out and no words on the screen. What it
  // does is what the same tick does on the two lists, and it is read there
  // from the column of boxes underneath it.
  head.setAttribute("aria-label", t("list.pick-all.aria"));
  head.checked = items.length > 0 && items.every(function (item) {
    return ticked(item.id);
  });

  head.addEventListener("change", function () {
    for (const one of boxes) {
      one.box.checked = head.checked;
      hold(one.id, head.checked);
    }
  });

  return {
    head: head,
    box: function (item) {
      const box = document.createElement("input");

      box.type = "checkbox";
      box.dataset.field = "assign-" + item.id;
      box.setAttribute("aria-label", t("hosts.assign-pick-row.aria", { id: item.id }));
      box.checked = ticked(item.id);
      box.addEventListener("change", function () {
        hold(item.id, box.checked);
        head.checked = boxes.every(function (one) {
          return one.box.checked;
        });
      });

      boxes.push({ box: box, id: item.id });

      return box;
    }
  };
}

// servicePortAssignRow is the cells of one service port in that panel: which
// one it is, where it is, what it was written down as, and how far it reaches
// on this Host.
//
// The tick of the row is not in here. It is in the column the head of the
// table opened, which is where the tick of a row is on the two lists as well,
// and assignPickColumn builds it.
//
// The scope list is registered as the row is built, so the press that sets one
// scope over everything ticked can write the value into the control that is on
// the screen rather than drawing the page again.
function servicePortAssignRow(item, picks) {
  const scope = listControl({
    options: bindScopeOptions(),
    value: item.id in picks.scoped ? picks.scoped[item.id] : picks.scopes[item.id]
  });

  scope.className = "assign-scope";
  scope.dataset.field = "scope-" + item.id;
  scope.disabled = !(item.id in picks.wanted ? picks.wanted[item.id] : picks.served[item.id]);
  // The name is on the control itself and not on a heading. The column says
  // what all of them are, and what tells one from the next is the service port
  // its row is about, which a reader going down the column cannot see.
  scope.setAttribute("aria-label", t("hosts.assign-row-scope.aria", { id: item.id }));
  scope.addEventListener("change", function () {
    picks.scoped[item.id] = scope.value;
  });

  picks.lists[item.id] = scope;

  // The address is a value and not a sentence, so it is put together here
  // rather than being a catalog string a translator is handed with two numbers
  // in it. It is one cell because it is read as one thing, and nothing in it
  // is broken across lines: the column is as wide as the longest address, and
  // the table is scrolled sideways where the panel is narrower than that.
  const service = item.service_ip + ":" + item.service_port;

  const description = item.description === undefined || item.description === null
    ? ""
    : String(item.description);

  return [item.id, service, item.local_port, description, scope];
}

// saveHostServicePorts sends what was ticked and what was rescoped, as the
// change it is.
//
// A panel that was not changed sends nothing at all. The request would carry
// empty lists, write no row and answer that it wrote none, so the round trip
// decides nothing; the panel closes and the list behind it is drawn again,
// which is what a save does.
//
// A refusal leaves the panel up with the ticks in it. They are the operator's
// work, several pages of it, and a panel that closed on a refusal would throw
// that away along with the chance to put right whatever was wrong.
async function saveHostServicePorts(host, picks, button, close, problem) {
  const add = [];
  const remove = [];
  const rescope = [];

  for (const id of Object.keys(picks.served)) {
    const carried = picks.served[id];
    const ticked = id in picks.wanted ? picks.wanted[id] : carried;
    const scope = id in picks.scoped ? picks.scoped[id] : picks.scopes[id];

    if (ticked && !carried) {
      add.push({ id: Number(id), scope: scope });
    } else if (!ticked && carried) {
      remove.push(Number(id));
    } else if (ticked && scope !== picks.scopes[id]) {
      // An assignment that is already there is named here and nowhere else.
      // The API leaves one it is only asked to add exactly as it is, on
      // purpose, so a row moves when and only when the request points at it.
      rescope.push({ id: Number(id), scope: scope });
    }
  }

  // The change goes out as one request per scope. A request carries a single
  // bind_scope, which is what its added rows are written on and what the ones
  // it names are moved to, and one press of Save can hold both: a service port
  // pinned to the Host itself and another left open are two scopes in the same
  // piece of work.
  //
  // A second request refused after the first one landed leaves half the change
  // stored. The panel stays up holding what it held, and it has not read the
  // list again, so pressing Save once more sends both requests again: adding an
  // assignment that is already there writes nothing, and moving one to the
  // scope it is already on writes the value it is already carrying. Sending the
  // half that landed a second time leaves the same rows behind.
  const changes = [];

  for (const scope of [bindScopeWildcard, bindScopeLoopback]) {
    const change = {
      add: idsScoped(add, scope),
      rescope: idsScoped(rescope, scope),
      bind_scope: scope
    };

    if (change.add.length > 0 || change.rescope.length > 0) {
      changes.push(change);
    }
  }

  if (changes.length === 0 && remove.length === 0) {
    setToast(function () {
      return t("hosts.assign-unchanged.notice", { id: host.id });
    });

    close("unchanged");

    return;
  }

  // The removals ride on the first request there is, since taking an
  // assignment away says nothing about a scope. Where there is no other
  // request, they are the whole of one.
  if (changes.length === 0) {
    changes.push({ add: [], rescope: [], bind_scope: bindScopeWildcard });
  }

  changes[0].remove = remove;

  // The button is held down for the whole call. The panel stays up while it is
  // in flight, which is an invitation to press again, and the second press
  // would send the same change a second time.
  button.disabled = true;
  problem.hidden = true;

  // The counts come from the answers, because they are counted over the rows
  // that were written and not over the requests: a service port that is already
  // assigned is asked for again without a row being written.
  let added = 0;
  let removed = 0;
  let rescoped = 0;

  try {
    for (const change of changes) {
      const answer = await apiCall("PUT", "/api/host/" + host.id + "/service-port", change);

      added += countedRows(answer, "added");
      removed += countedRows(answer, "removed");
      rescoped += countedRows(answer, "rescoped");
    }

    setToast(function () {
      return rescoped > 0
        ? t("hosts.assign-scoped.notice",
          { id: host.id, added: added, removed: removed, rescoped: rescoped })
        : t("hosts.assign-saved.notice",
          { id: host.id, added: added, removed: removed });
    });

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

// idsScoped are the identifiers of the entries that carry one scope, in the
// order they were read, which is the order of the list they came off.
function idsScoped(entries, scope) {
  return entries.filter(function (entry) {
    return entry.scope === scope;
  }).map(function (entry) {
    return entry.id;
  });
}

// countedRows is one of the counts an answer carries. An answer that does not
// carry it is read as nought rather than as the number of identifiers that were
// sent: what was asked for is not what was written, which is the whole reason
// the answer carries counts of its own.
function countedRows(answer, name) {
  return answer === null || typeof answer[name] !== "number" ? 0 : answer[name];
}

// sayInPanel puts a line inside the panel that is not a refusal. It is brought
// into view for the reason showPanelProblem does it: the press that wrote it
// may be above or below where the operator is looking.
function sayInPanel(said, message) {
  said.textContent = message;
  said.hidden = false;
  said.scrollIntoView({ block: "nearest" });
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

// localForwardBusy are the states a local forward passes through on its way to
// an answer. The panel reads its list again while a row is in one of them,
// because nothing else on the screen would say that the row has settled.
const localForwardBusy = ["starting", "reconnecting"];

// openHostLocalForwards puts up the panel that lists the local forwards of a
// Host a page at a time, and adds, changes, switches and deletes them.
//
// A local forward runs the other way from a service port: this machine opens
// the port, and what connects to it is carried over the SSH connection of the
// Host to an address the Host reaches. It has a panel of its own for that
// reason, since a local port and an address mean a different machine on each.
//
// The list is drawn the way the service port list is, ticks and the presses
// over them included. The add and the edit forms go in a second panel over
// this one, so the page of the list stays where it was while one is up.
async function openHostLocalForwards(host) {
  // A page of its own, for the reason the host key panel has one.
  const page = { number: 1, size: listSizes[0] };

  listPicks["local-forwards"] = {};

  const list = document.createElement("div");

  list.className = "assign-list";
  list.dataset.list = "local-forwards";

  // Why a press was refused, inside the panel for the reason the service port
  // panel puts it there: the line above the screen is behind the backdrop.
  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "local-forwards";
  problem.hidden = true;

  let shown = [];
  let total = 0;
  let open = true;
  let timer = null;

  // What the last flip over the ticks could not change. It stays under the
  // presses through the reads that follow and goes with the next press or a
  // turn of the page, which is when the rows it names may no longer be here.
  let refusals = [];

  // The page the list on the screen was read from. A read that fails puts the
  // numbers back to it, as the host key panel does.
  let drawn = { number: page.number, size: page.size };

  // describe is how a row is named outside the table: in the confirmation of a
  // batch delete and in the list of what a flip left as it was.
  function describe(item) {
    return t("local-forwards.picked-row.text",
      { port: item.local_port, target: item.target_ip + ":" + item.target_port });
  }

  function heading() {
    const panel = document.querySelector("[data-modal-panel=\"local-forwards\"]");

    return panel === null ? null : panel.querySelector("h2");
  }

  function drawList() {
    // The press that set a draw off is usually inside the list it replaces, so
    // the keyboard is noted and put back the way paint does it for a screen.
    const focused = focusOf(list);

    list.textContent = "";

    keepPicksOnPage("local-forwards", shown);

    // The Add is at the far end of the row that turns the page, over the list
    // it adds to, and is there on an empty list too.
    const head = document.createElement("div");

    head.className = "list-head";

    const controls = pageControls("local-forwards", page, total, turnPage);
    if (controls !== null) {
      head.appendChild(controls);
    }

    head.appendChild(actionButton(t("common.add.button"), "local-forward-add", function () {
      return openForm(null);
    }, "primary list-add"));

    list.appendChild(head);

    if (shown.length === 0) {
      list.appendChild(statusLine(t("local-forwards.none.empty"), "empty"));
      putBackFocus(list, heading(), focused);

      return;
    }

    const picks = listPickColumn("local-forwards", shown, function (id) {
      const item = shown.find(function (one) {
        return one.id === id;
      });

      return t("local-forwards.pick-row.aria", { port: item.local_port });
    });

    const table = buildTable(
      [t("local-forwards.local-port.column"), t("local-forwards.scope.column"),
        t("local-forwards.target.column"), t("local-forwards.description.column"),
        t("local-forwards.status.column"), ""],
      shown.map(function (item) {
        const row = localForwardRow(item, openForm, flipOne, remove);

        row.pick = picks.box(item.id);

        return row;
      }),
      [0],
      picks.head
    );

    const bar = deletePickedBar({
      name: "local-forwards",
      items: shown,
      table: table,
      label: t("local-forwards.delete-picked.button"),
      title: t("local-forwards.delete-picked.title"),
      text: t("local-forwards.delete-picked.text"),
      describe: describe,
      path: function (item) {
        return "/api/local-forward/" + item.id;
      },
      said: function (count) {
        return t(plural(count, "local-forwards.deleted-picked-one.notice",
          "local-forwards.deleted-picked-many.notice"), { count: count });
      },
      partly: function (deleted, failed) {
        return t("local-forwards.deleted-picked-some.notice", { deleted: deleted, failed: failed });
      },
      draw: function () {
        return reread(false);
      }
    });

    // In front of the delete, for the reason the Host list puts its own two
    // there: these can be taken back and the delete cannot.
    const flips = document.createDocumentFragment();

    flips.appendChild(ticksWakeThePress("local-forwards", table,
      actionButton(t("local-forwards.enable-picked.button"), "local-forwards-enable-picked",
        function () {
          return flipPicked(true);
        })));
    flips.appendChild(ticksWakeThePress("local-forwards", table,
      actionButton(t("local-forwards.disable-picked.button"), "local-forwards-disable-picked",
        function () {
          return flipPicked(false);
        })));

    bar.insertBefore(flips, bar.firstChild);

    list.appendChild(bar);

    if (refusals.length > 0) {
      const refused = document.createElement("div");

      refused.className = "picked-list";
      refused.dataset.list = "local-forwards-flip-picked";

      for (const refusal of refusals) {
        refused.appendChild(pickedDeleteRow({ describe: describe }, refusal.item, refusal.reason));
      }

      list.appendChild(refused);
    }

    list.appendChild(table);

    putBackFocus(list, heading(), focused);
  }

  async function readList() {
    const answer = await apiCall("GET",
      "/api/host/" + host.id + "/local-forward?" + pageQuery(page));

    takeListPage(page, answer);

    shown = answer === null || answer.items === null || answer.items === undefined
      ? []
      : answer.items;
    total = answer === null || typeof answer.total !== "number" ? shown.length : answer.total;

    drawList();
  }

  // settle decides whether the list is read again. soon is a write that has
  // just gone out: the server starts the forward after it answers, so the row
  // it answered with has not yet left the state it was stored in.
  function settle(soon) {
    if (timer !== null) {
      window.clearTimeout(timer);
      timer = null;
    }

    if (!open) {
      return;
    }

    const busy = shown.some(function (item) {
      return localForwardBusy.indexOf(item.status) !== -1;
    });

    if (!soon && !busy) {
      return;
    }

    timer = window.setTimeout(function () {
      timer = null;

      run(function () {
        return reread(false);
      });
    }, statusRefreshMs);
  }

  async function reread(soon) {
    try {
      await readList();
    } catch (error) {
      if (error instanceof Redirected) {
        throw error;
      }

      page.number = drawn.number;
      page.size = drawn.size;

      showPanelProblem(problem, error.message);

      return;
    }

    drawn = { number: page.number, size: page.size };

    settle(soon);
  }

  function turnPage() {
    refusals = [];
    problem.hidden = true;

    return reread(false);
  }

  // refuse says why a press was refused the way a screen says it, on the
  // window and on a line that stays. The line is the one in the panel the
  // press was made in, since the one above the screen is behind the backdrop.
  function refuse(error, line) {
    if (error instanceof Redirected) {
      throw error;
    }

    const say = sayOf(error);

    showPanelProblem(line, say());
    showToast(say, "error");
  }

  // openForm puts the add form, or the edit form of item, in a panel over
  // this one. A refusal leaves it up with what was typed still in it.
  function openForm(item) {
    const said = element("p", "");

    said.className = "notice error";
    said.dataset.problem = "local-forward-form";
    said.hidden = true;

    let close = null;

    const form = localForwardForm(item, function (values) {
      return save(item, values, said, close);
    }, function () {
      close(null);
    });

    return openModal({
      name: item === null ? "local-forward-add" : "local-forward-edit",
      title: t("local-forwards.panel.title", { id: host.id, ip: host.ip }),
      body: [said, form],
      opened: function (shut) {
        close = shut;
      }
    });
  }

  async function save(item, values, said, close) {
    const body = localForwardBody(values);

    said.hidden = true;

    try {
      if (item === null) {
        await apiCall("POST", "/api/host/" + host.id + "/local-forward", body);
      } else {
        await apiCall("PUT", "/api/local-forward/" + item.id, body);
      }
    } catch (error) {
      refuse(error, said);

      return;
    }

    close("saved");

    setToast(function () {
      return t(item === null ? "local-forwards.added.notice" : "local-forwards.updated.notice",
        { port: body.local_port });
    });

    // The list is in the order of ids and a new row has the highest, so it is
    // on the last page.
    if (item === null) {
      page.number = lastPageOf(total + 1, page.size);
    }

    return reread(true);
  }

  async function flipOne(item) {
    const on = !item.enabled;

    problem.hidden = true;

    try {
      await apiCall("PUT", "/api/local-forward/" + item.id, localForwardFlipBody(item, on));
    } catch (error) {
      refuse(error, problem);

      return;
    }

    setToast(function () {
      return t(on ? "local-forwards.now-enabled.notice" : "local-forwards.now-disabled.notice",
        { port: item.local_port });
    });

    return reread(true);
  }

  // flipPicked is flipPickedHosts over the ticked rows of this page, counted
  // and reported the same way.
  async function flipPicked(on) {
    const wanted = pickedIDs("local-forwards");
    const chosen = shown.filter(function (item) {
      return wanted.indexOf(item.id) !== -1;
    });

    if (chosen.length === 0) {
      return;
    }

    problem.hidden = true;
    refusals = [];

    let flipped = 0;
    let already = 0;

    for (const item of chosen) {
      if (item.enabled === on) {
        already += 1;

        continue;
      }

      try {
        await apiCall("PUT", "/api/local-forward/" + item.id, localForwardFlipBody(item, on));
      } catch (error) {
        if (error instanceof Redirected) {
          throw error;
        }

        refusals.push({ item: item, reason: error.message });

        continue;
      }

      flipped += 1;
    }

    if (refusals.length > 0) {
      const counts = { flipped: flipped, already: already, refused: refusals.length };
      const say = function () {
        return t(on ? "local-forwards.enabled-picked-some.notice"
          : "local-forwards.disabled-picked-some.notice", counts);
      };

      showPanelProblem(problem, say());
      showToast(say, "error");
    } else if (already > 0) {
      setToast(function () {
        return t(on ? "local-forwards.enabled-picked-same.notice"
          : "local-forwards.disabled-picked-same.notice", { flipped: flipped, already: already });
      });
    } else {
      setToast(function () {
        return t(on
          ? plural(flipped, "local-forwards.enabled-picked-one.notice",
            "local-forwards.enabled-picked-many.notice")
          : plural(flipped, "local-forwards.disabled-picked-one.notice",
            "local-forwards.disabled-picked-many.notice"),
        { count: flipped });
      });
    }

    return reread(true);
  }

  async function remove(item) {
    if (!window.confirm(t("local-forwards.delete.confirm", { port: item.local_port }))) {
      return;
    }

    problem.hidden = true;

    try {
      await apiCall("DELETE", "/api/local-forward/" + item.id);
    } catch (error) {
      refuse(error, problem);

      return;
    }

    delete listPicks["local-forwards"][String(item.id)];

    setToast(function () {
      return t("local-forwards.deleted.notice", { port: item.local_port });
    });

    return reread(false);
  }

  // The list is read before the panel goes up, so that a refusal is answered
  // with the line above the screen rather than with an empty panel.
  await readList();

  drawn = { number: page.number, size: page.size };

  const panel = openModal({
    name: "local-forwards",
    title: t("local-forwards.panel.title", { id: host.id, ip: host.ip }),
    body: [
      element("p", t("local-forwards.panel.text")),
      problem,
      list
    ],
    buttons: [
      { label: t("common.close.button"), name: "close" }
    ]
  });

  settle(false);

  await panel;

  open = false;
  settle(false);
}

// localForwardRow is the cells of one local forward in that panel. The last
// error goes under the row, as it does on the status screen, because a
// sentence in a column of a table this narrow is a column of single words.
function localForwardRow(item, edit, flip, remove) {
  const buttons = document.createElement("div");

  buttons.className = "buttons";
  buttons.appendChild(actionButton(t("common.edit.button"), "local-forward-edit-" + item.id, function () {
    return edit(item);
  }));
  buttons.appendChild(actionButton(
    item.enabled ? t("local-forwards.disable.button") : t("local-forwards.enable.button"),
    "local-forward-toggle-" + item.id,
    function () {
      return flip(item);
    }
  ));
  buttons.appendChild(actionButton(t("common.delete.button"), "local-forward-delete-" + item.id, function () {
    return remove(item);
  }, "danger"));

  const description = item.description === undefined || item.description === null
    ? ""
    : String(item.description);

  const row = {
    cells: [
      item.local_port,
      localForwardScopeText(item.bind_scope),
      item.target_ip + ":" + item.target_port,
      description,
      statusBadge(item.status),
      buttons
    ]
  };

  const failure = typeof item.last_error === "string" ? item.last_error : "";
  if (failure !== "") {
    row.under = element("span", failure);
    row.under.className = "last-error";
  }

  return row;
}

// localForwardScopeOptions are the two scopes a local forward can be opened
// on. They are the words bindScopeOptions sends, under other names: there the
// port is opened on the Host, and here it is opened on this machine.
function localForwardScopeOptions() {
  return [
    { value: bindScopeWildcard, text: t("local-forwards.scope-wildcard.option") },
    { value: bindScopeLoopback, text: t("local-forwards.scope-loopback.option") }
  ];
}

function localForwardScopeText(value) {
  const stored = bindScopeStored(value);
  const option = localForwardScopeOptions().find(function (one) {
    return one.value === stored;
  });

  return option.text;
}

// localForwardForm is the add form when item is null and the edit form of item
// otherwise. The two ask for the same values, because the update takes the
// whole record the way the update of a service port does.
function localForwardForm(item, onSubmit, onCancel) {
  const stored = item === null ? {} : item;

  const localPort = portField("local_port", t("local-forwards.local-port.label"), stored.local_port);

  localPort.note = t("local-forwards.local-port.hint");

  const targetIP = ipField("target_ip", t("local-forwards.target-ip.label"), stored.target_ip);

  targetIP.note = t("local-forwards.target-ip.hint");

  const spec = {
    name: item === null ? "local-forward-create" : "local-forward-edit",
    legend: item === null
      ? t("local-forwards.add.title")
      : t("local-forwards.edit.title", { port: item.local_port }),
    submitLabel: item === null ? t("common.add.button") : t("common.save.button"),
    fields: [
      localPort,
      {
        name: "bind_scope",
        label: t("local-forwards.scope.label"),
        value: bindScopeStored(stored.bind_scope),
        options: localForwardScopeOptions(),
        note: t("local-forwards.scope.hint")
      },
      targetIP,
      portField("target_port", t("local-forwards.target-port.label"), stored.target_port),
      { name: "description", label: t("local-forwards.description.label"), value: stored.description }
    ],
    onSubmit: onSubmit
  };

  if (onCancel !== undefined) {
    spec.onCancel = onCancel;
  }

  return buildForm(spec);
}

function localForwardBody(values) {
  return {
    bind_scope: values.bind_scope,
    local_port: asNumber(values.local_port),
    target_ip: values.target_ip.trim(),
    target_port: asNumber(values.target_port),
    description: values.description
  };
}

// localForwardFlipBody is a stored local forward switched on or off. The update
// takes the whole record, so the row goes back as it was read with enabled
// beside it, and the edit form leaves enabled out so as to keep it.
function localForwardFlipBody(item, on) {
  const body = localForwardBody(item);

  body.enabled = on;

  return body;
}

// assignPickedToHosts is the second press over the ticks of the service port
// list: the ticked service ports given to several Hosts in one go.
//
// It is the other direction of the panel a Host opens. That one asks which
// service ports one Host carries, and until now there was nothing going the
// other way: a service port wanted on twenty Hosts was twenty panels, each of
// them a list to find one row in.
//
// The ticked rows are read again here rather than taken from the button, for
// the reason deletePicked reads them again: the button was drawn with the page
// and the ticks have been changing since.
//
// The list is drawn again however the panel ended, because a Host that took the
// service ports carries them now and the Assign button of that Host's row opens
// on a different set.
async function assignPickedToHosts(items, draw) {
  const wanted = pickedIDs("service-ports");
  const chosen = items.filter(function (item) {
    return wanted.indexOf(item.id) !== -1;
  });

  if (chosen.length === 0) {
    return;
  }

  const outcome = await pickHostsToAssignTo(chosen);

  // The ticks are what the press was made of, and the press has landed. Left
  // where they are, the bar over the list would still be awake over work that
  // is done, which reads as work still to do. A run that was refused in part
  // leaves them, since that run is not over: the panel stays up holding the
  // Hosts that are left.
  if (outcome === "assigned") {
    for (const item of chosen) {
      delete listPicks["service-ports"][String(item.id)];
    }
  }

  return draw();
}

// pickHostsToAssignTo puts up the panel that says which Hosts are to carry the
// service ports that were ticked.
//
// There is no confirmation over it and this panel is why: an assignment can be
// taken away again from the panel of the Host it was given to, so nothing here
// is beyond recall, and the step before the press is the picking itself. What
// a confirmation would add is a second reading of a list the operator has just
// built.
//
// What it hands back is "assigned" where every Host took the change, and
// nothing where the panel was closed or where something was refused.
async function pickHostsToAssignTo(ports) {
  // The panel has a page of its own and does not touch listPages, the way the
  // panel a Host opens does: the service port list behind it stays on the page
  // and at the size it was left on.
  const page = { number: 1, size: listSizes[0] };

  // The Hosts that are ticked, held against the row each was ticked on rather
  // than as identifiers alone. A Host that refuses the change is named back to
  // the operator by its address, and by then the page it was ticked on may be
  // several pages away.
  const picks = {};

  const list = document.createElement("div");

  list.className = "assign-list";
  list.dataset.list = "assign-picked-hosts";

  // Why a page or a send was refused. It is inside the panel because the line
  // over the screen is behind the backdrop, where whoever pressed the button
  // cannot read it.
  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "assign-picked-hosts";
  problem.hidden = true;

  // The Hosts that would not take the change, each with what it met. It is the
  // shape the confirmation of a batch delete names its rows in.
  const refused = document.createElement("div");

  refused.className = "picked-list";
  refused.dataset.list = "assign-picked-hosts-refused";
  refused.hidden = true;

  // What is about to be given away, named the way the list behind the panel
  // names it. The ticks were made on a table of seven columns and the panel is
  // over it, so this is the one place the operator can read back what the
  // press is carrying.
  const carrying = document.createElement("div");

  carrying.className = "picked-list";
  carrying.dataset.list = "assign-picked-ports";

  for (const port of ports) {
    carrying.appendChild(pickedAssignRow(port.id, t("service-ports.picked-row.text",
      { id: port.id, ip: port.service_ip, port: port.service_port }), null));
  }

  // How far the assignments this press writes are opened. It is asked for here
  // rather than left at the wildcard because the two other places that assign
  // in bulk both ask for it - the Host form with every service port ticked, and
  // the service port form with every Host ticked - and because the reach of a
  // batch written without it is put right one Host at a time, in the panels
  // this press exists to save. The wildcard is what it opens on, which is what
  // the column stores when a request leaves the field out.
  const scope = listControl({ options: bindScopeOptions(), value: bindScopeWildcard });

  scope.className = "assign-bulk-scope";
  scope.dataset.field = "assign-picked-scope";
  scope.setAttribute("aria-label", t("service-ports.assign-picked-scope.label"));

  const scopeRow = document.createElement("div");

  scopeRow.className = "assign-bulk";

  const scopeLabel = element("span", t("service-ports.assign-picked-scope.label"));

  scopeLabel.className = "assign-bulk-label";
  scopeRow.appendChild(scopeLabel);
  scopeRow.appendChild(scope);

  // The rows of the page on the screen, and how long the Host list is.
  let shown = [];
  let total = 0;

  // settle is the rule the send is under: it is dead while no Host is ticked,
  // which is the state the panel opens in. The button belongs to openModal and
  // not to this list, so it is found through the panel this list is in.
  // openModal builds the whole of the panel before it hands its promise back,
  // so the button is there from the first call of this to the last; a call made
  // before that, which cannot happen from a tick, finds nothing and leaves the
  // button as it was.
  function settle() {
    const panel = list.closest(".modal-panel");
    if (panel === null) {
      return;
    }

    const send = panel.querySelector("[data-action=\"assign-picked-hosts-send\"]");
    if (send !== null) {
      send.disabled = Object.keys(picks).length === 0;
    }
  }

  function drawList() {
    // Only the list is built again, as in the panel a Host opens: nothing here
    // writes to #app, so a draw of the screen behind the backdrop cannot take
    // the panel down and this cannot draw over it.
    list.textContent = "";

    if (shown.length === 0) {
      list.appendChild(statusLine(t("hosts.none.empty"), "empty"));

      return;
    }

    const controls = pageControls("assign-picked-hosts", page, total, turnPage);
    if (controls !== null) {
      list.appendChild(controls);
    }

    for (const host of shown) {
      list.appendChild(hostPickRow(host, picks, settle));
    }
  }

  async function drawPage() {
    const answer = await apiCall("GET", "/api/host?" + pageQuery(page));

    takeListPage(page, answer);

    shown = answer === null || answer.items === null || answer.items === undefined
      ? []
      : answer.items;
    total = answer === null || typeof answer.total !== "number" ? shown.length : answer.total;

    drawList();
  }

  // drawn is the page the list on the screen was built from. A page that could
  // not be fetched leaves the list as it was, so the numbers are put back to
  // it: left where the press moved them, the next press would step over a page
  // that was never read.
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

  // The first page is fetched before the panel goes up, so a refusal is
  // answered with the line over the screen rather than with an empty panel.
  await drawPage();

  drawn = { number: page.number, size: page.size };

  const opened = openModal({
    name: "assign-picked-hosts",
    title: t("service-ports.assign-picked.title"),
    body: [
      element("p", t("service-ports.assign-picked.text")),
      carrying,
      element("p", t("service-ports.assign-picked-hosts.text")),
      element("p", t("service-ports.assign-picked-scope.text")),
      scopeRow,
      problem,
      refused,
      list
    ],
    buttons: [
      {
        label: t("service-ports.assign-picked-send.button"),
        name: "send",
        variant: "primary",
        press: function (button, close) {
          return sendPickedAssignments(ports, picks, scope.value, button, close,
            problem, refused, drawList, settle);
        }
      },
      { label: t("common.close.button"), name: "close" }
    ]
  });

  // The panel is on the screen by now, and nothing in it is ticked, so this is
  // what puts the send to sleep before it can be pressed.
  settle();

  return opened;
}

// hostPickRow is one Host in that panel: the box and which Host it is, by the
// same identifier and address the list names it by.
//
// The box and the text are inside a label, so the whole row is the press. A
// checkbox on its own is a target the width of a character, which is the one
// thing a list ticked on a phone cannot be. There is no control beside it, so
// the row is the label and nothing has to be kept outside it.
//
// A tick writes the map the send reads and nothing else: it fetches nothing,
// and the list is not drawn again, so a Host ticked here is still ticked after
// the page has been turned and turned back.
function hostPickRow(host, picks, settle) {
  const row = document.createElement("label");

  row.className = "assign-row assign-pick";
  row.dataset.assign = String(host.id);

  const box = document.createElement("input");

  box.type = "checkbox";
  box.dataset.field = "assign-host-" + host.id;
  box.checked = String(host.id) in picks;

  const text = document.createElement("span");

  text.className = "assign-text";
  text.appendChild(element("span", t("hosts.picked-row.text", { id: host.id, ip: host.ip })));

  const description = host.description === undefined || host.description === null
    ? ""
    : String(host.description);

  if (description !== "") {
    const saidAbout = element("small", description);

    saidAbout.className = "assign-said";
    text.appendChild(saidAbout);
  }

  box.addEventListener("change", function () {
    if (box.checked) {
      picks[String(host.id)] = host;
    } else {
      delete picks[String(host.id)];
    }

    settle();
  });

  row.appendChild(box);
  row.appendChild(text);

  return row;
}

// pickedAssignRow is one line of the two lists that panel holds which are not
// ticked: the service ports about to be given away, and the Hosts that would
// not take them. It is built the way the confirmation of a batch delete builds
// its rows, down to the classes, so a row named to the operator reads the same
// in both places.
function pickedAssignRow(id, text, reason) {
  const row = document.createElement("div");
  const bad = reason !== null && reason !== undefined;

  row.className = bad ? "picked-row bad" : "picked-row";
  row.dataset.picked = String(id);
  row.appendChild(element("span", text));

  if (bad) {
    const said = element("p", reason);

    said.className = "picked-said";
    row.appendChild(said);
  }

  return row;
}

// sendPickedAssignments gives the ticked service ports to the ticked Hosts, one
// Host at a time.
//
// One request per Host and not one for the batch, for the reason the batch
// delete sends one at a time: the database runs on a single connection, so a
// request that wrote a hundred rows in one transaction would hold it for the
// whole of them and the reconcile pass would wait behind it.
//
// The whole ticked set goes to every Host, the service ports it carries already
// included, and nothing is read first to find out which those are. The API
// leaves an assignment that is already there exactly as it is - a request that
// only adds does not touch the reach of one that is stored - so a Host carrying
// half of them is given the other half and no row is written twice. What comes
// back is counted over rows written, which is how the line at the end can say
// how many were there already.
//
// A refusal stops that Host and nothing else. The Hosts are separate changes,
// and one that is gone, or that another screen is holding, says nothing about
// the next; what was asked for was the rest of the list as much as that Host.
async function sendPickedAssignments(ports, picks, scope, button, close, problem,
  refused, redraw, settle) {
  const hosts = Object.keys(picks).map(Number).sort(function (one, other) {
    return one - other;
  }).map(function (id) {
    return picks[String(id)];
  });

  // Nothing is ticked, which is the state the send is dead in. It is read
  // again here rather than trusted to the button.
  if (hosts.length === 0) {
    return;
  }

  const add = ports.map(function (port) {
    return port.id;
  });

  // Held down for the whole run rather than for one request, because the panel
  // stays up until the last of them is answered and a second press would send
  // the list again from the top.
  button.disabled = true;
  problem.hidden = true;
  refused.hidden = true;

  const failures = [];
  let written = 0;
  let reached = 0;

  try {
    for (const host of hosts) {
      let answer = null;

      try {
        answer = await apiCall("PUT", "/api/host/" + host.id + "/service-port",
          { add: add, bind_scope: scope });
      } catch (error) {
        if (error instanceof Redirected) {
          // The session ended and the page is on its way to the login. What is
          // left of the list is not sent after it.
          close(null);

          throw error;
        }

        failures.push({ host: host, reason: error.message });

        continue;
      }

      reached += 1;
      written += countedRows(answer, "added");

      // The Host has the service ports, so the tick for it goes here. A run
      // after a partial one is then over the Hosts that are left and not over
      // the ones that are already carrying them.
      delete picks[String(host.id)];
    }
  } finally {
    settle();
  }

  if (failures.length === 0) {
    setToast(function () {
      return t("service-ports.assigned-picked.notice",
        { added: written, hosts: reached, already: reached * add.length - written });
    });

    close("assigned");

    return;
  }

  // Something was refused, so the panel stays up with those Hosts and what each
  // of them met. The list of Hosts is drawn again with it: the ones that took
  // the change are no longer ticked, and a box still ticked for one of them
  // would be read as work that has not happened.
  refused.textContent = "";

  for (const failure of failures) {
    refused.appendChild(pickedAssignRow(failure.host.id,
      t("hosts.picked-row.text", { id: failure.host.id, ip: failure.host.ip }),
      failure.reason));
  }

  refused.hidden = false;
  redraw();

  showPanelProblem(problem, t("service-ports.assigned-picked-some.notice",
    { added: written, reached: reached, failed: failures.length }));
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

  keepPicksOnPage("service-ports", ports);

  const nodes = [
    editing === undefined ? servicePortCreateForm() : servicePortEditForm(editing)
  ];

  if (ports.length === 0) {
    nodes.push(statusLine(t("service-ports.none.empty"), "empty"));
  } else {
    const controls = pageControls("service-ports", page, total, drawServicePorts);
    if (controls !== null) {
      nodes.push(controls);
    }

    const picks = listPickColumn("service-ports", ports, function (id) {
      return t("service-ports.pick-row.aria", { id: id });
    });

    const table = buildTable(
      [t("service-ports.id.column"), t("service-ports.service-ip.column"),
        t("service-ports.service-port.column"), t("service-ports.local-port.column"),
        t("service-ports.description.column"), t("service-ports.updated.column"), ""],
      ports.map(function (port) {
        return { cells: servicePortRow(port), pick: picks.box(port.id) };
      }),
      [0, 2, 3],
      picks.head
    );

    const bar = deletePickedBar({
      name: "service-ports",
      items: ports,
      table: table,
      label: t("service-ports.delete-picked.button"),
      title: t("service-ports.delete-picked.title"),
      text: t("service-ports.delete-picked.text"),
      describe: function (port) {
        return t("service-ports.picked-row.text",
          { id: port.id, ip: port.service_ip, port: port.service_port });
      },
      path: function (port) {
        return "/api/service-port/" + port.id;
      },
      said: function (count) {
        return t(plural(count, "service-ports.deleted-picked-one.notice",
          "service-ports.deleted-picked-many.notice"), { count: count });
      },
      partly: function (deleted, failed) {
        return t("service-ports.deleted-picked-some.notice",
          { deleted: deleted, failed: failed });
      },
      draw: drawServicePorts
    });

    // The second press over the same ticks, beside the one that deletes them.
    // It is on this list alone, so it is put in here rather than handed to the
    // bar: the Host list has no press of the kind to be given.
    bar.appendChild(ticksWakeThePress("service-ports", table,
      actionButton(t("service-ports.assign-picked.button"), "service-ports-assign-picked",
        function () {
          return assignPickedToHosts(ports, drawServicePorts);
        })));

    nodes.push(bar, table);
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
      portField("local_port", t("service-ports.local-port.label"), undefined,
        privilegedPortAdvice),
      { name: "description", label: t("service-ports.description.label") },
      // The other half of the pair on the host form, ticked to begin with for
      // the same reason, and on the add form alone for the same reason.
      {
        name: "assign_to_all_hosts",
        label: t("service-ports.assign-all.label"),
        type: "checkbox",
        value: true,
        note: t("service-ports.assign-all.hint")
      },
      // The scope of this batch of assignments, asked for on the same terms as
      // on the host form. The two answers mean the same thing on every Host the
      // batch reaches, which is what lets one of them be picked for all of them
      // at once: the loopback addresses and the wildcards are on every machine,
      // while an address of one interface is on one.
      bindScopeField(undefined, { field: "assign_to_all_hosts", ticked: true })
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
      portField("local_port", t("service-ports.local-port.label"), port.local_port,
        privilegedPortAdvice),
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

  // And the scope rides on the assignment, so it goes out only where there is
  // one to land on, as it does on the host form.
  if (values.assign_to_all_hosts) {
    body.bind_scope = values.bind_scope;
  }

  await apiCall("POST", "/api/service-port", body);

  setToast(function () {
    return t("service-ports.added.notice",
      { ip: body.service_ip, port: body.service_port });
  });

  return drawServicePorts();
}

async function updateServicePort(port, values) {
  await apiCall("PUT", "/api/service-port/" + port.id, servicePortBody(values));

  editingServicePortID = null;
  setToast(function () {
    return t("service-ports.updated.notice", { id: port.id });
  });

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

  setToast(function () {
    return t("service-ports.deleted.notice", { id: port.id });
  });

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

  const nodes = [logControls(answer), logScope()];

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

  // The two halves are laid out in the direction of the page, and on a page
  // that reads right to left that puts the clock before the date. A timestamp
  // reads left to right whatever the page does, and it says so for itself
  // rather than being joined into one run, since the break between the two
  // halves is the point of the two spans.
  node.dir = "ltr";

  const text = value === null || value === undefined ? "" : String(value);
  const split = text.indexOf("T");

  // Anything that is not the shape this writes is left as it stands. A line
  // that could not be parsed carries whatever it carried.
  if (split === -1) {
    node.textContent = text;

    return node;
  }

  // The space between the two halves is what the line breaks at when the
  // column is narrow, and what keeps the date and the clock apart when it is
  // not: two boxes set side by side with nothing between them touch.
  node.appendChild(element("span", text.slice(0, split)));
  node.appendChild(document.createTextNode(" "));
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

  // The sentence is drawn from the identifier the line carries rather than from
  // the words in the file, so the screen is in the language it is being read in
  // while the file stays in the one it is grepped in. A line that names no
  // sentence is shown as it was written.
  cell.appendChild(element("span", logLineText(line)));

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
function logControls(answer) {
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

  // The press that empties the log is offered only where the read came back.
  // A read that did not is a log this server is not writing to a file at all,
  // or one whose file it could not open, and in both there is nothing here to
  // empty: the button would be asking for a password to do nothing with.
  if (answer !== null && answer !== undefined) {
    const clear = actionButton(t("logs.clear.button"), "log-clear", logClearPanel, "danger");

    row.appendChild(clear);
  }

  return row;
}

// logClearPanel asks before the log is emptied, and takes the password of the
// account.
//
// The password is asked for because what this does cannot be taken back. It is
// the line the uninstall is on rather than the line the restart is on: after a
// restart the service is running again, and after this the lines that were in
// the file are gone.
//
// What the warning has to say beside that is what is left behind. Only the file
// this server is writing to is emptied, and the rotated files beside it stay as
// the retention settings keep them, so an operator pressing this to make the
// machine forget something would otherwise be left believing it had.
async function logClearPanel() {
  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "log-clear";
  problem.hidden = true;

  const password = accountPasswordField("log-clear-password",
    t("logs.clear-password.label"), t("logs.clear-password.hint"));

  let cleared = false;

  await openModal({
    name: "log-clear",
    title: t("logs.clear-confirm.title"),
    body: [
      element("p", t("logs.clear-confirm.text")),
      element("p", t("logs.clear-kept.text")),
      problem,
      password.row
    ],
    buttons: [
      {
        label: t("logs.clear-confirm.button"),
        name: "clear",
        // Painted as what cannot be taken back, the way the list that replaces
        // a trusted host key is.
        variant: "danger",
        press: function (node, close) {
          return sendLogClear(password, node, close, problem, function () {
            cleared = true;
          });
        }
      },
      { label: t("common.cancel.button"), name: "cancel" }
    ]
  });

  if (!cleared) {
    return;
  }

  // The message says it happened, because the screen it is drawn over is a log
  // with nothing in it, which is the same thing the screen shows when the level
  // filter matches none of the lines.
  setToast(function () {
    return t("logs.cleared.notice");
  });

  return drawLogs();
}

// sendLogClear is the press at the bottom of that panel.
//
// A refusal is shown inside the panel and the panel stays up, with what was
// typed still in it. The password box is the thing most likely to be refused,
// and a panel that went away would take the box with it.
async function sendLogClear(password, button, close, problem, done) {
  // An empty box is answered here rather than by a round trip, the way a form
  // answers a value the server would refuse anyway.
  if (password.input.value === "") {
    password.input.classList.add("bad");
    showPanelProblem(problem, t("logs.clear-password.error"));

    return;
  }

  password.input.classList.remove("bad");
  problem.hidden = true;
  button.disabled = true;

  try {
    await apiCall("POST", "/api/logs/clear", { password: password.input.value });
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    // Nothing was emptied, so the panel goes back to offering the press.
    button.disabled = false;

    showPanelProblem(problem, error.message);

    return;
  }

  done();
  close("clear");
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
  settingsImportResult = null;

  return drawSettings();
}

// drawUpdate is the Update screen: what is running, what the newest release is,
// and the two settings that decide whether either of those is looked at again.
//
// It does not look while it is being drawn. What it shows is what the timer in
// the server last found, so opening the screen costs nothing on the far side
// and two people opening it do not make two requests of GitHub. The press is
// there for somebody who wants the answer now.
async function drawUpdate() {
  const update = await apiCall("GET", "/api/update");
  const set = await apiCall("GET", "/api/settings");

  const versions = updateVersions(update);

  // The presses and what is said about them go inside the card they are about.
  // Left beside it they line up against the edge of the page while the cards
  // above and below them are inset, and a row that is the only thing on the
  // page not in a card reads as having come loose from one.
  const buttons = document.createElement("div");
  buttons.className = "buttons";

  const check = actionButton(t("update.check.button"), "update-check", function () {
    return submitUpdateCheck(check);
  });

  buttons.appendChild(check);

  // The press that installs is offered where the release is newer and where
  // this installation is one an install can be run on. Drawn otherwise it
  // would be a button whose only answer is a refusal.
  if (update.installable && update.newer && update.comparable) {
    buttons.appendChild(actionButton(t("update.install.button"), "update-install",
      function () {
        return updateInstallPanel(update);
      }, "danger"));
  }

  versions.appendChild(buttons);

  // Why the install is not among them, under the row it is missing from.
  if (!update.installable) {
    versions.appendChild(statusLine(t("update.not-installable.notice"), "warning"));
  }

  render(t("update.screen.title"), [
    element("p", t("update.screen.text")),
    versions,
    updateSettingsCard(set)
  ]);
}

// updateSettingsCard is the two switches and the interval, on the screen they
// are about rather than among the twenty settings on the other one.
//
// They are saved through the same call the Settings screen saves through, so
// there is one place that writes settings and one place that refuses a value.
// What is sent is the whole set with these three changed, for the reason the
// service port edit sends the whole record: the call replaces what is stored.
function updateSettingsCard(set) {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "update-settings";
  card.appendChild(element("h2", t("update.settings.title")));

  card.appendChild(buildForm({
    name: "update-settings",
    submitLabel: t("common.save.button"),
    fields: [
      {
        name: "update_check_enabled",
        label: t("update.check-enabled.label"),
        type: "checkbox",
        value: set.update_check_enabled,
        note: t("update.check-enabled.hint")
      },
      {
        name: "update_check_interval_hours",
        label: t("update.interval.label"),
        value: set.update_check_interval_hours,
        inputMode: "numeric",
        filter: portCharacters,
        check: checkUpdateInterval,
        note: t("update.interval.hint")
      },
      {
        name: "update_auto_install",
        label: t("update.auto-install.label"),
        type: "checkbox",
        value: set.update_auto_install,
        note: t("update.auto-install.hint"),
        // The one switch on this screen that takes the service down when it
        // acts. What it does is said beside it rather than left to the word
        // "automatic", which reads as a convenience.
        advise: function (ticked) {
          return ticked ? t("update.auto-install.notice") : "";
        }
      }
    ],
    onSubmit: function (values) {
      return saveUpdateSettings(set, values);
    }
  }));

  return card;
}

// checkUpdateInterval holds the box to what the server takes. A zero would be a
// timer rearming as fast as it can against an API that counts requests, which
// is why it is the one number a box left empty must not send.
function checkUpdateInterval(value) {
  const hours = Number(String(value).trim());

  if (!Number.isInteger(hours) || hours < 1 || hours > 8760) {
    return t("update.interval.error");
  }

  return "";
}

async function saveUpdateSettings(set, values) {
  const body = Object.assign({}, set, {
    update_check_enabled: values.update_check_enabled,
    update_check_interval_hours: asNumber(values.update_check_interval_hours),
    update_auto_install: values.update_auto_install
  });

  const data = await apiCall("PUT", "/api/settings", body);

  saySettingsSaved(data, "update.settings-saved.notice", "settings.saved-next-start.notice");

  return drawUpdate();
}

// updateVersions is the pair of versions and the sentence about them.
function updateVersions(update) {
  const card = document.createElement("section");

  card.className = "card";
  card.dataset.card = "update-versions";

  const counts = document.createElement("div");
  counts.className = "counts";
  // The running version is drawn with the v the tag carries. The two boxes sit
  // side by side and are compared at a glance, and one written 3.8.0 against
  // one written v3.8.0 reads as two different things. The constant itself is
  // left alone: what the API answers is the version as this program holds it,
  // and the v belongs to how a release is named.
  counts.appendChild(countBox(t("update.running.label"), versionTag(update.version),
    "update-running"));
  counts.appendChild(countBox(t("update.latest.label"),
    update.tag === "" ? t("update.latest-unknown.text") : update.tag, "update-latest"));
  card.appendChild(counts);

  // A check that failed is not a version that is up to date. It is said as its
  // own line so that the two are never read as each other.
  if (update.problem !== "") {
    card.appendChild(statusLine(t("update.check-failed.notice", { reason: update.problem }),
      "warning"));
  } else if (update.tag === "") {
    card.appendChild(statusLine(t("update.never-checked.empty"), "empty"));
  } else if (!update.comparable) {
    card.appendChild(statusLine(t("update.not-comparable.notice", { tag: update.tag }), "warning"));
  } else if (update.newer) {
    card.appendChild(statusLine(t("update.newer.notice", { tag: update.tag }), "warning"));
  } else {
    card.appendChild(statusLine(t("update.current.notice"), "ok"));
  }

  // formatTime answers the word for never on the zero time, which is what the
  // server sends where nothing has looked yet. That case is the empty line
  // above, so the sentence is left off here rather than reading "last looked:
  // never" under it.
  const when = formatTime(update.checked_at);
  if (when !== t("common.never.text")) {
    card.appendChild(element("p", t("update.checked.text", { when: when })));
  }

  return card;
}

// versionTag writes a version the way a release tag is written. A value that
// already carries the v is left as it is, so this says the same thing whether
// it is handed the constant or a tag.
function versionTag(version) {
  const said = String(version === null || version === undefined ? "" : version).trim();

  if (said === "" || said.charAt(0) === "v") {
    return said;
  }

  return "v" + said;
}

// submitUpdateCheck looks now and draws what came back.
async function submitUpdateCheck(button) {
  button.disabled = true;

  try {
    await apiCall("POST", "/api/update/check");
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    button.disabled = false;

    // The screen is drawn again even though the check failed, because what the
    // server now holds is that failure and the screen is where it is said.
    await drawUpdate();

    throw error;
  }

  setToast(function () {
    return t("update.checked.notice");
  });

  return drawUpdate();
}

// updateInstallPanel asks before the executable is replaced.
//
// It takes the password of the account, the way the uninstall and the log
// emptying do: what it starts cannot be stopped from here, and it ends with
// every tunnel coming down.
async function updateInstallPanel(update) {
  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "update-install";
  problem.hidden = true;

  const password = accountPasswordField("update-install-password",
    t("update.password.label"), t("update.password.hint"));

  let started = false;

  await openModal({
    name: "update-install",
    title: t("update.confirm.title"),
    body: [
      element("p", t("update.confirm.text", { tag: update.tag })),
      element("p", t("update.confirm-restart.text")),
      problem,
      password.row
    ],
    buttons: [
      {
        label: t("update.install.button"),
        name: "install",
        variant: "danger",
        press: function (node, close) {
          return sendUpdateInstall(password, node, close, problem, function () {
            started = true;
          });
        }
      },
      { label: t("common.cancel.button"), name: "cancel" }
    ]
  });

  if (!started) {
    return;
  }

  // Written here and not where started is set, so that what is left behind is
  // left behind by an install the server took and by nothing else. A password
  // that was refused never gets this far, and a mark it wrote would be a result
  // waiting for the next load of a page where nothing was installed at all.
  markUpdateStarted(update.tag);

  // drawRestarting is not reused here: it draws only over the Settings screen
  // (screens.js, its first line), and it counts down a wait the server named.
  // An install never names one, because what it is waiting on is a download and
  // a service manager rather than a delay this end was told about.
  return waitOutTheInstall();
}

// waitOutTheInstall keeps the screen up until a different version answers, and
// then loads the page again.
//
// What it watches for is the version and not whether the server answers at all.
// The service is stopped only after the release has been fetched and checked,
// so for the whole of the download it is the process being replaced that
// answers, and a page that took an answer for the install being over would
// reload onto the version it started from and call it done.
//
// The version this end already has is the one the corner was drawn from. It is
// read before the wait starts rather than during it, because during it is when
// it changes.
async function waitOutTheInstall() {
  const was = loadedVersion;
  const until = Date.now() + updateWaitLimitSec * 1000;

  drawUpdateStarted(0);

  while (Date.now() < until) {
    await pause(updateWaitEverySec * 1000);

    // The operator went somewhere else. What is drawn there is theirs, and a
    // page that reloaded out from under them would take away whatever they
    // were in the middle of.
    if (currentScreen !== "update") {
      return;
    }

    drawUpdateStarted(Math.min(1, (updateWaitLimitSec * 1000 - (until - Date.now())) / (updateWaitLimitSec * 1000)));

    if (await theVersionChanged(was)) {
      break;
    }
  }

  if (currentScreen !== "update") {
    return;
  }

  // The page is loaded again whether a new version answered or the wait ran
  // out. Where it answered this shows the install that went through; where it
  // did not, it shows the version that is still running, which is what says the
  // install did not take. Neither is a screen left saying something is still
  // going on when nothing is.
  window.location.reload();
}

// theVersionChanged is one ask of the path the corner is drawn from.
//
// It needs no session, so it keeps answering across the restart that takes
// every session with it, and it is the same file for every client. Anything
// other than a version that is there and is not the one this page started on
// reads as not yet: a refusal, a connection that did not open, a body that is
// not what it should be. Each of those is a moment during the install, and the
// next ask is two seconds away.
async function theVersionChanged(was) {
  try {
    const response = await fetch(versionPath, {
      headers: { Accept: "application/json" },
      credentials: "same-origin",
      cache: "no-store"
    });

    if (!response.ok) {
      return false;
    }

    const payload = await response.json();

    return typeof payload.version === "string" && payload.version !== "" &&
      payload.version !== was;
  } catch (error) {
    return false;
  }
}

// drawUpdateStarted is the screen left up while the install runs. filled is how
// much of the wait has gone by, from 0 to 1.
//
// The bar is how long this page keeps waiting and not how far the install has
// got. Nothing here is told that: the process that would say it is the one
// being replaced. So the bar is drawn as what it is, a wait with an end to it,
// and the sentence beside it says as much.
function drawUpdateStarted(filled) {
  render(t("update.started.title"), [
    statusLine(t("update.started.notice"), "info"),
    element("p", t("update.started.text")),
    waitingBar(filled),
    element("p", t("update.started-waiting.text", { seconds: updateWaitLimitSec })),
    element("p", t("update.started-reload.text"))
  ]);
}

// waitingBar is the bar itself.
//
// It is a pair of elements rather than <progress>, because what a browser draws
// for that one is its own and cannot be made to match the rest of these
// screens. The outer element carries the role and the numbers, so what a screen
// reader is told is the same as what is drawn.
//
// The width is written per ask rather than moved by a transition. A transition
// would keep the bar going while the page waits on a request that may not
// answer, which is motion saying something is happening when nothing is known
// to be.
function waitingBar(filled) {
  const bar = document.createElement("div");
  const fill = document.createElement("div");
  const percent = Math.round(Math.max(0, Math.min(1, filled)) * 100);

  bar.className = "waiting-bar";
  bar.setAttribute("role", "progressbar");
  bar.setAttribute("aria-valuemin", "0");
  bar.setAttribute("aria-valuemax", "100");
  bar.setAttribute("aria-valuenow", String(percent));
  bar.setAttribute("aria-label", t("update.started-progress.aria"));

  fill.className = "waiting-bar-fill";
  fill.style.width = percent + "%";

  bar.appendChild(fill);

  return bar;
}

async function sendUpdateInstall(password, button, close, problem, done) {
  if (password.input.value === "") {
    password.input.classList.add("bad");
    showPanelProblem(problem, t("update.password.error"));

    return;
  }

  password.input.classList.remove("bad");
  problem.hidden = true;
  button.disabled = true;

  try {
    await apiCall("POST", "/api/update/install", { password: password.input.value });
  } catch (error) {
    if (error instanceof Redirected) {
      throw error;
    }

    button.disabled = false;

    showPanelProblem(problem, error.message);

    return;
  }

  done();
  close("install");
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
    // The example path is handed over whole rather than built in the sentence
    // out of the directory and a tail written into the catalog. A value is laid
    // out as one run and what is written round it is not, so on a page that
    // reads right to left the tail was carried off and the path was drawn in
    // pieces, with its end before its beginning.
    return t("settings.path-windows.hint", {
      what: whatItIs,
      dir: dir,
      example: dir.replace(/[\\/]+$/, "") + "\\logs\\tunnel-manager.log"
    });
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
      },
      {
        name: "ui_default_language",
        label: t("settings.ui-language.label"),
        value: set.ui_default_language,
        options: languageOptions(),
        note: t("settings.ui-language.hint")
      }
    ],
    onSubmit: saveSettings
  });
}

// languageOptions is what the language of the installation is picked from: the
// languages the UI is drawn in, each under the name it calls itself by, and
// above them the pick that names none.
//
// The name of a language is not translated, for the reason the list in the
// corner does not translate it either: somebody looking for their own language
// is looking for the word they would write it with.
//
// The empty value is a value and not a gap. It is what says this installation
// names no language, which leaves every browser being shown the one it asks
// for, and it is what the setting holds until somebody picks otherwise.
function languageOptions() {
  const options = [{ value: "", text: t("settings.ui-language-any.option") }];

  for (const language of languages) {
    options.push({ value: language.code, text: language.name });
  }

  return options;
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
    logging_file_compress: values.logging_file_compress,
    ui_default_language: values.ui_default_language
  };

  return sendSettings(body);
}

// sendSettings stores what the form holds. A port a local forward opens is
// answered with the panel that moves one of the two, and the body is sent
// again once it has: with the port picked there for this server, or as it was
// when the forward is the one that moved.
async function sendSettings(body) {
  let data;

  try {
    data = await apiCall("PUT", "/api/settings", body);
  } catch (error) {
    if ((error.code !== settingsAPIPortTakenCode && error.code !== settingsAPIPortSocksCode) ||
      error.data === null) {
      throw error;
    }

    const picked = await apiPortTakenPanel(error.data, true);
    if (picked === null) {
      return;
    }

    return sendSettings(picked.api === null ? body : Object.assign({}, body, { api_port: picked.api }));
  }

  saySettingsSaved(data, "settings.saved.notice", "settings.saved-next-start.notice");

  // A save that changed the language of the installation changes what this
  // screen is drawn in, unless the operator has picked a language here. It is
  // read again rather than taken from the answer, so the one place that decides
  // what the language is stays the one that asked the server for it.
  await followInstallationLang();

  return drawSettings();
}

// apiPortTakenPanel puts up what a port asked for as api_port and opened by a
// local forward is answered with. It settles with null for a cancel, with
// { api: port } where the port of this server is to move, and with
// { api: null } once the forward has moved to the port that was picked. The
// port of this server is offered only to a save: an import names its port in
// the file, which this screen does not change.
async function apiPortTakenPanel(taken, offerAPI) {
  const forward = taken.local_forward;
  const socks = taken.socks_host === null || taken.socks_host === undefined ? null : taken.socks_host;
  const suggested = typeof taken.suggested_port === "number" && taken.suggested_port > 0
    ? String(taken.suggested_port) : "";

  const problem = element("p", "");

  problem.className = "notice error";
  problem.dataset.problem = "api-port-taken";
  problem.hidden = true;

  const choices = [];

  function choice(value, label) {
    const row = document.createElement("div");

    row.className = "port-choice";

    const pick = document.createElement("label");
    const radio = document.createElement("input");

    radio.type = "radio";
    radio.name = "api-port-taken";
    radio.value = value;
    radio.dataset.choice = value;
    radio.checked = choices.length === 0;

    pick.appendChild(radio);
    pick.appendChild(document.createTextNode(label));

    const port = textControl({ value: suggested, hint: t("form.port-example.hint"),
      inputMode: "numeric", filter: portCharacters });

    port.dataset.field = value + "_port";
    port.setAttribute("aria-label", label);

    // Typing a port is picking the way it belongs to.
    port.addEventListener("focus", function () {
      radio.checked = true;
    });

    row.appendChild(pick);
    row.appendChild(port);

    choices.push({ value: value, radio: radio, port: port, row: row });
  }

  if (offerAPI) {
    choice("api", t("settings.api-port-taken-api.option"));
  }

  if (socks === null) {
    choice("forward", t("settings.api-port-taken-forward.option"));
  } else {
    choice("socks", t("settings.api-port-taken-socks.option"));
  }

  // With one way out there is nothing to pick between, so the radio is not
  // drawn and the box stands under its sentence.
  if (choices.length === 1) {
    choices[0].radio.hidden = true;
  }

  let picked = null;

  await openModal({
    name: "api-port-taken",
    title: socks === null
      ? t("settings.api-port-taken.title")
      : t("settings.api-port-taken-socks.title"),
    body: [
      socks === null
        ? element("p", t("settings.api-port-taken.text", {
          port: forward.local_port,
          host: forward.host_ip === "" ? String(forward.host_id) : forward.host_ip,
          target: forward.target_ip + ":" + forward.target_port
        }))
        : element("p", t("settings.api-port-taken-socks.text", {
          port: socks.socks_port,
          host: socks.host_ip === "" ? String(socks.host_id) : socks.host_ip
        })),
      problem
    ].concat(choices.map(function (one) {
      return one.row;
    })),
    buttons: [
      {
        label: offerAPI ? t("settings.api-port-taken-save.button") : t("settings.api-port-taken-import.button"),
        name: "apply",
        press: async function (node, close) {
          const chosen = choices.find(function (one) {
            return one.radio.checked;
          });
          const said = checkPort(chosen.port.value);

          if (said !== "") {
            showPanelProblem(problem, said);

            return;
          }

          const port = asNumber(chosen.port.value);

          if (chosen.value === "api") {
            picked = { api: port };
            close("apply");

            return;
          }

          problem.hidden = true;
          node.disabled = true;

          try {
            if (socks === null) {
              await moveLocalForward(forward.id, port);
            } else {
              await apiCall("PUT", "/api/host/" + socks.host_id, { socks_port: port });
            }
          } catch (error) {
            if (error instanceof Redirected) {
              throw error;
            }

            node.disabled = false;
            showPanelProblem(problem, error.message);

            return;
          }

          picked = { api: null };
          close("apply");
        }
      },
      { label: t("common.cancel.button"), name: "cancel" }
    ]
  });

  return picked;
}

// moveLocalForward gives a local forward another local port and leaves the
// rest of it as it is stored. It is read first because the update takes the
// whole record, and a scope left out of it would be stored as the wildcard.
async function moveLocalForward(id, port) {
  const item = await apiCall("GET", "/api/local-forward/" + id);

  return apiCall("PUT", "/api/local-forward/" + id,
    localForwardBody(Object.assign({}, item, { local_port: String(port) })));
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
  const data = await apiCall("PUT", "/api/settings", { api_https_enabled: enabled });

  // Both sides of the answer say the same thing here. This setting is read when
  // the listener is opened and nowhere else, so a change to it is never in
  // place before the next start.
  saySettingsSaved(data, "certificate.https-saved.notice", "certificate.https-saved.notice");

  return drawSettings();
}

// saySettingsSaved says what a save of the settings did, out of what came back
// from it rather than out of the fact that it did not fail.
//
// The three screens that write settings send different parts of the same row
// and so have different words for a change that is in place, but the answer
// they read is the same one: the server names what it changed and says whether
// any of it waits for a restart. A save that changed nothing is the case worth
// telling apart, since the press was the operator asking whether what is on the
// screen is what is stored, and being told it was stored answers a different
// question.
function saySettingsSaved(data, inPlace, atNextStart) {
  const changes = data === null || data.changes === null || data.changes === undefined
    ? []
    : data.changes;

  if (changes.length === 0) {
    setToast(function () {
      return t("settings.saved-nothing.notice");
    });

    return;
  }

  setToast(function () {
    return t(data.restart_required ? atNextStart : inPlace);
  });
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
// sentence about the connections that are already open comes from the server,
// named so that it is said in the language of the page: it is the answer to "I
// pressed it and the browser still shows the old certificate", which is what
// happens every time.
function certificateReplacement(result) {
  const wrap = document.createElement("div");

  if (typeof result.note === "string" && result.note !== "") {
    wrap.appendChild(statusLine(serverText(result.note, result.note_code, null), "warning"));
  }

  if (typeof result.warning === "string" && result.warning !== "") {
    wrap.appendChild(statusLine(
      serverText(result.warning, result.warning_code, result.warning_args), "warning"));
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

  setToast(function () {
    return t("certificate.renewed.notice");
  });

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
    setFailure(sayOf(error));

    return drawSettings();
  }

  certificateDraft = { certPEM: "", keyPEM: "" };
  certificateProblem = "";
  certificateReplaceResult = answer;

  setToast(function () {
    return t("certificate.installed.notice");
  });

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
    setFailure(function () {
      return t("account.nothing.error");
    });

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

  setToast(function () {
    return accountOutcome(data);
  });

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

  if (settingsImportResult !== null) {
    intro.push(transferItemsTable(settingsImportResult));
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
  const forwards = countOf(data, "local_forwards");

  setToast(function () {
    const said = [t(plural(hosts,
      plural(ports, "transfer.exported-host-one-port-one.notice",
        "transfer.exported-host-one-port-many.notice"),
      plural(ports, "transfer.exported-host-many-port-one.notice",
        "transfer.exported-host-many-port-many.notice")),
    { name: name, hosts: hosts, ports: ports })];

    // The local forwards are a sentence of their own rather than a third count
    // in the one above, which would take the four sentences to eight. A file
    // with none says nothing about them, as a file from before they existed.
    if (forwards > 0) {
      said.push(t(plural(forwards, "transfer.exported-local-forwards-one.notice",
        "transfer.exported-local-forwards-many.notice"), { count: forwards }));
    }

    return said.join(" ");
  });

  // The screen is drawn again, which is what takes the password out of the box
  // it was typed into. Nothing on the screen repeats it.
  return drawSettings();
}

async function exportSettings(values) {
  const data = await apiCall("POST", "/api/export/settings", { password: values.password });
  const name = handTheFileOut(data, "settings");

  setToast(function () {
    return t("transfer.exported-settings.notice", { name: name });
  });

  return drawSettings();
}

// countOf reads one of the counts an answer carries: what an export wrote, and
// what the status screen says is waiting. A count the server left out is said
// as none rather than as the word undefined, which is what an older server
// that does not send it reads as.
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
    setFailure(sayOf(error));

    return drawSettings();
  }

  transferDraft.tunnels = "";
  transferProblem.tunnels = "";
  transferResult = answer;

  setToast(function () {
    return importOutcome(answer);
  });

  return drawSettings();
}

// importOutcome is the message after an import. What became of each row is in
// the table on the card; this is the count, and what to do about what was
// skipped.
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
      const reason = item.reason === null || item.reason === undefined ? "" : item.reason;

      return [
        transferItemKind(item.kind),
        serverText(item.name, item.name_code, item.name_values),
        transferItemAction(item.action),
        serverText(reason, item.reason_code, item.reason_values)
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

  if (kind === "local_forward") {
    return t("transfer.kind-local-forward.text");
  }

  if (kind === "assignment") {
    return t("transfer.kind-assignment.text");
  }

  if (kind === "setting") {
    return t("transfer.kind-setting.text");
  }

  if (kind === "socks") {
    return t("transfer.kind-socks.text");
  }

  return kind === null || kind === undefined ? "" : String(kind);
}

// transferItemAction names what the import did with a row. An action this
// version does not know is shown as the server wrote it, for the reason
// transferItemKind gives.
function transferItemAction(action) {
  if (action === "added") {
    return t("transfer.action-added.text");
  }

  if (action === "replaced") {
    return t("transfer.action-replaced.text");
  }

  if (action === "skipped") {
    return t("transfer.action-skipped.text");
  }

  return action === null || action === undefined ? "" : String(action);
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

    // A port of the file that a local forward opens here is offered the one
    // way out an import has, moving the forward, and the same file is sent
    // again with the password still held in values and nowhere else.
    if ((error.code === importAPIPortTakenCode || error.code === importAPIPortSocksCode) &&
      error.data !== null) {
      const picked = await apiPortTakenPanel(error.data, false);
      if (picked !== null) {
        return importSettings(values);
      }
    }

    transferProblem.settings = error.message;
    settingsImportResult = null;
    setFailure(sayOf(error));

    return drawSettings();
  }

  transferDraft.settings = "";
  transferProblem.settings = "";
  settingsImportResult = answer !== null && answer !== undefined &&
    Array.isArray(answer.items) && answer.items.length > 0 ? answer : null;

  setToast(function () {
    return settingsImportOutcome(answer);
  });

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

  setToast(function () {
    return t("restart.back.notice");
  });

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

  // The stored port can be held by another program when the new image comes
  // up, and the service then tries the port it ran on before, which is the port
  // this page is on, before one the system picks. That port is the one address
  // that can be asked: it is this page's own
  // origin, so the answer means something. Asked after the grace above, it is
  // the new image that answers and not the one that was going down. Where it
  // does not answer, the page goes to the stored address as it always did,
  // since that one cannot be asked from here.
  if (leavesThisPort(address) && await serviceAnswers()) {
    if (currentScreen !== "settings") {
      return;
    }

    setToast(function () {
      return t("restart.back.notice");
    });

    return drawSettings();
  }

  if (currentScreen !== "settings") {
    return;
  }

  drawRestarting(answer, "", address, 0);
  window.location.assign(address);
}

// leavesThisPort says whether the address the service comes back at is on
// another port than this page. A move of the scheme alone keeps the port, and
// an answer on it is then from a server this page cannot read.
function leavesThisPort(address) {
  return new URL(address).port !== window.location.port;
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
  navigate("uninstalled", {
    say: function () {
      return t("uninstall.done.notice");
    },
    kind: "info"
  });
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
        return [serverText(file.what, file.what_code, null), file.path];
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
        return [serverText(file.what, file.what_code, null), file.path, file.error];
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
    manualForward(),
    manualSocks(),
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
    manualTopology(),
    flow,
    t("manual.one-tunnel-dials.text"),
    t("manual.one-tunnel-columns.text"),
    manualExample()
  ]);
}

// The example every part of the drawing is labelled with. One Host, one service
// port, and the numbers stay the same on the drawing, in the list under it and
// in the command at the end, so the reader can follow one port from box to box.
// The addresses are the documentation ranges, so they name no real machine.
const manualExampleHost = "203.0.113.10";
const manualExampleUser = "deploy";
const manualExampleService = "198.51.100.50:8080";
const manualExampleLocalPort = "18080";

// manualTopology is the drawing of the machines: the Host with its SSH server
// and the port that is opened on it, this machine with tunnel-manager on it, the
// service off to the side, a client on the far side, and the four legs the
// traffic takes, numbered in the order they happen.
//
// The service is drawn outside the frame of this machine on purpose. It is any
// address tunnel-manager can open a connection to, which is usually another
// machine on the same network and not this one, and a service box sitting
// inside the frame taught the opposite.
//
// It is an SVG rather than boxes because the point of it is where the machines
// are and which way each connection is opened, and boxes in a row cannot show
// that the client and tunnel-manager both go to the Host and neither goes to
// the other. The words inside it are kept to names and addresses, because a
// line of SVG text does not wrap, and what each leg does is said in the list
// under the drawing, which does. On a narrow screen it scrolls sideways rather
// than shrinking until the addresses cannot be read.
function manualTopology() {
  const wrap = document.createElement("div");

  wrap.className = "topology";
  // Addresses and the numbers read left to right whatever the page does, and
  // the drawing is laid out for that reading.
  wrap.dir = "ltr";

  const svg = svgElement("svg", {
    viewBox: "0 0 900 330",
    role: "img",
    "aria-label": t("manual.diagram.aria")
  });

  const defs = svgElement("defs");
  const marker = svgElement("marker", {
    id: "topology-arrow",
    viewBox: "0 0 10 10",
    refX: "9",
    refY: "5",
    markerWidth: "7",
    markerHeight: "7",
    orient: "auto-start-reverse"
  });

  marker.appendChild(svgElement("path", { d: "M 0 0 L 10 5 L 0 10 z", class: "topology-head" }));
  defs.appendChild(marker);
  svg.appendChild(defs);

  // The two machines, drawn as the frames the boxes sit in.
  svg.appendChild(topologyFrame(200, 30, 230, 260, t("manual.diagram-host.label") + " " +
    manualExampleHost));
  svg.appendChild(topologyFrame(480, 30, 210, 260, t("manual.diagram-here.label")));

  // What is on each machine, and the service, which is on neither.
  svg.appendChild(topologyBox(20, 125, 140, 60, t("manual.diagram-client.label"), ""));
  svg.appendChild(topologyBox(225, 75, 180, 60, t("manual.diagram-sshd.label"), ":22"));
  svg.appendChild(topologyBox(225, 200, 180, 60, t("manual.diagram-listener.label"),
    "0.0.0.0:" + manualExampleLocalPort));
  svg.appendChild(topologyBox(500, 75, 170, 60, "tunnel-manager", ""));
  svg.appendChild(topologyBox(720, 200, 170, 60, t("manual.diagram-service.label"),
    manualExampleService));

  // 1. tunnel-manager opens the SSH connection, so the arrow starts at it.
  svg.appendChild(topologyLeg("M 500 105 L 405 105", 1, 452, 105, "topology-ssh"));
  // 2. A client connects to the port on the Host.
  svg.appendChild(topologyLeg("M 160 155 L 185 155 L 185 230 L 225 230", 2, 185, 192, ""));
  // 3. That connection runs back down the SSH connection to tunnel-manager.
  svg.appendChild(topologyLeg("M 405 230 L 585 230 L 585 135", 3, 500, 230, "topology-back"));
  // 4. tunnel-manager opens its own connection to the service, which is why
  //    that leg leaves the frame of this machine.
  svg.appendChild(topologyLeg("M 670 105 L 805 105 L 805 200", 4, 805, 150, ""));

  wrap.appendChild(svg);

  return wrap;
}

// svgElement is element for the SVG namespace, which createElement does not
// put a tag in, so an svg made with it draws nothing.
function svgElement(tag, attributes) {
  const node = document.createElementNS("http://www.w3.org/2000/svg", tag);

  for (const name of Object.keys(attributes || {})) {
    node.setAttribute(name, attributes[name]);
  }

  return node;
}

// topologyFrame is the outline of one machine, with its name along the top
// edge. The name sits outside the frame, above it, so a long one in another
// language runs past the edge rather than being cut by it.
function topologyFrame(x, y, width, height, title) {
  const group = svgElement("g", { class: "topology-frame" });

  group.appendChild(svgElement("rect", { x, y, width, height, rx: "8" }));

  const label = svgElement("text", { x: x + 12, y: y - 8, class: "topology-frame-title" });

  label.textContent = title;
  group.appendChild(label);

  return group;
}

// topologyBox is one thing on a machine: its name, and under it the address or
// the port it is at, where it has one.
function topologyBox(x, y, width, height, title, address) {
  const group = svgElement("g", { class: "topology-box" });

  group.appendChild(svgElement("rect", { x, y, width, height, rx: "6" }));

  const name = svgElement("text", {
    x: x + width / 2,
    y: address === "" ? y + height / 2 + 5 : y + 24,
    class: "topology-box-title"
  });

  name.textContent = title;
  group.appendChild(name);

  if (address !== "") {
    const where = svgElement("text", { x: x + width / 2, y: y + 45, class: "topology-box-address" });

    where.textContent = address;
    group.appendChild(where);
  }

  return group;
}

// topologyLeg is one connection: the line it takes, with its arrowhead at the
// end it is opened towards, and the number of the step it is in a circle on the
// line.
function topologyLeg(path, step, cx, cy, variant, marker) {
  const group = svgElement("g", { class: ("topology-leg " + variant).trim() });
  const head = marker === undefined ? "topology-arrow" : marker;

  group.appendChild(svgElement("path", { d: path, "marker-end": "url(#" + head + ")" }));
  group.appendChild(svgElement("circle", { cx, cy, r: "11" }));

  const number = svgElement("text", { x: cx, y: cy + 4, class: "topology-step" });

  number.textContent = String(step);
  group.appendChild(number);

  return group;
}

// manualExample is the drawing in words: the same Host, port and service, the
// four legs in the order they are numbered, and the ssh command that would open
// the same tunnel by hand, for the reader who knows that command already.
function manualExample() {
  const example = document.createElement("div");

  example.dataset.example = "tunnel";
  const names = {
    host: manualExampleHost,
    user: manualExampleUser,
    service: manualExampleService,
    local_port: manualExampleLocalPort
  };

  example.appendChild(element("p", t("manual.example-intro.text", names)));

  const steps = document.createElement("ol");

  // The four keys are written out because the check that every key in the
  // catalog is asked for by some script reads the scripts for them as written.
  steps.appendChild(element("li", t("manual.example-step-1.text", names)));
  steps.appendChild(element("li", t("manual.example-step-2.text", names)));
  steps.appendChild(element("li", t("manual.example-step-3.text", names)));
  steps.appendChild(element("li", t("manual.example-step-4.text", names)));
  example.appendChild(steps);
  example.appendChild(element("p", t("manual.example-command.text")));

  const command = element("code", "ssh -N -R 0.0.0.0:" + manualExampleLocalPort + ":" +
    manualExampleService + " " + manualExampleUser + "@" + manualExampleHost);

  const block = element("pre");

  // A command reads left to right whatever the page does, and it starts at
  // the left edge of its box rather than being set against the right one.
  block.dir = "ltr";
  block.appendChild(command);
  example.appendChild(block);

  return example;
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
    t("manual.parts-disabling.text"),
    t("manual.parts-local-forward.text"),
    t("manual.parts-socks.text")
  ]);
}

const manualForwardHost = "203.0.113.10";
const manualForwardPort = "15432";
const manualForwardTarget = "198.51.100.20:5432";

function manualForward() {
  const names = { host: manualForwardHost, port: manualForwardPort, target: manualForwardTarget };
  const steps = document.createElement("ol");

  steps.appendChild(element("li", t("manual.forward-step-1.text", names)));
  steps.appendChild(element("li", t("manual.forward-step-2.text", names)));
  steps.appendChild(element("li", t("manual.forward-step-3.text", names)));
  steps.appendChild(element("li", t("manual.forward-step-4.text", names)));

  return manualCard("forward", t("manual.forward.title"), [
    t("manual.forward-direction.text"),
    manualForwardTopology(),
    t("manual.forward-example.text", names),
    steps,
    t("manual.forward-where.text"),
    t("manual.forward-scope.text"),
    t("manual.forward-port.text"),
    t("manual.forward-status.text"),
    bulletList([
      t("manual.forward-status-connected.text"),
      t("manual.forward-status-reconnecting.text"),
      t("manual.forward-status-error.text"),
      t("manual.forward-status-disabled.text")
    ]),
    t("manual.forward-sshd.text")
  ]);
}

// manualForwardTopology is manualTopology drawn the other way round: the two
// machines trade places, so the port a client reaches is on this one and the
// connection leaves from the Host. It keeps its own arrowhead id because both
// drawings are on the same page.
function manualForwardTopology() {
  const wrap = document.createElement("div");

  wrap.className = "topology";
  wrap.dir = "ltr";

  const svg = svgElement("svg", {
    viewBox: "0 0 900 330",
    role: "img",
    "aria-label": t("manual.forward-diagram.aria")
  });

  const defs = svgElement("defs");
  const marker = svgElement("marker", {
    id: "forward-arrow",
    viewBox: "0 0 10 10",
    refX: "9",
    refY: "5",
    markerWidth: "7",
    markerHeight: "7",
    orient: "auto-start-reverse"
  });

  marker.appendChild(svgElement("path", { d: "M 0 0 L 10 5 L 0 10 z", class: "topology-head" }));
  defs.appendChild(marker);
  svg.appendChild(defs);

  svg.appendChild(topologyFrame(200, 30, 230, 260, t("manual.diagram-here.label")));
  svg.appendChild(topologyFrame(480, 30, 210, 260, t("manual.diagram-host.label") + " " +
    manualForwardHost));

  svg.appendChild(topologyBox(20, 200, 140, 60, t("manual.diagram-client.label"), ""));
  svg.appendChild(topologyBox(225, 75, 180, 60, "tunnel-manager", ""));
  svg.appendChild(topologyBox(225, 200, 180, 60, t("manual.forward-diagram-port.label"),
    "0.0.0.0:" + manualForwardPort));
  svg.appendChild(topologyBox(500, 75, 170, 60, t("manual.diagram-sshd.label"), ":22"));
  svg.appendChild(topologyBox(720, 200, 170, 60, t("manual.forward-diagram-target.label"),
    manualForwardTarget));

  svg.appendChild(topologyLeg("M 405 105 L 500 105", 1, 452, 105, "topology-ssh", "forward-arrow"));
  svg.appendChild(topologyLeg("M 160 230 L 225 230", 2, 192, 230, "", "forward-arrow"));
  svg.appendChild(topologyLeg("M 405 230 L 585 230 L 585 135", 3, 500, 230, "topology-back",
    "forward-arrow"));
  svg.appendChild(topologyLeg("M 670 105 L 805 105 L 805 200", 4, 805, 150, "", "forward-arrow"));

  wrap.appendChild(svg);

  return wrap;
}

const manualSocksAddress = "192.0.2.20:1080";

function manualSocksCommand(text) {
  const block = element("pre");

  block.dir = "ltr";
  block.appendChild(element("code", text));

  return block;
}

function manualSocks() {
  const browsers = document.createElement("ul");
  const chrome = document.createElement("li");

  browsers.appendChild(element("li", t("manual.socks-firefox.text")));
  chrome.appendChild(element("p", t("manual.socks-chrome.text")));
  chrome.appendChild(manualSocksCommand("google-chrome --proxy-server=\"socks5://" +
    manualSocksAddress + "\""));
  chrome.appendChild(element("p", t("manual.socks-chrome-loopback.text")));
  chrome.appendChild(manualSocksCommand("--proxy-bypass-list=\"<-loopback>\""));
  browsers.appendChild(chrome);

  return manualCard("socks", t("manual.socks.title"), [
    t("manual.socks-what.text"),
    t("manual.socks-switch.text"),
    t("manual.socks-open.text"),
    t("manual.socks-browser.text", { address: manualSocksAddress }),
    browsers,
    t("manual.socks-connect.text")
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
