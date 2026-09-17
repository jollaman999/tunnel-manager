"use strict";

// Where the UI is served from. The part of the path after it names the screen.
const uiPrefix = "/ui/";

// statusRefreshMs is how often the status screen asks again. It is the default
// period of the reconcile loop, so what is on the screen is never more than one
// pass of that loop behind what the server has done.
const statusRefreshMs = 5000;

// apiLoginPath and apiSetupPath are the two calls whose refusals must not be
// turned into a move to another screen. A 401 from the login is what wrong
// credentials look like, and a 403 from the setup would send the operator to
// the screen they are already on.
const apiLoginPath = "/api/login";
const apiSetupPath = "/api/setup";

// csrfCookieName and csrfHeaderName are the two ends of the CSRF check. The
// server hands the token of the session out in a cookie it leaves readable from
// here on purpose, and wants it back in a header, which a page on another
// origin cannot put on a request to this one without a preflight this server
// never answers.
const csrfCookieName = "tm_csrf";
const csrfHeaderName = "X-CSRF-Token";

// versionPath is where the number in the corner is read from. It is served from
// under /ui/ rather than from /api/, so the login screen, which has no session
// yet, can show it too.
const versionPath = "/ui/version.json";

// The bounds the API takes a port in. The server refuses anything outside them,
// and the same bounds are checked here so that a typo is reported next to the
// box it was typed in instead of after a round trip.
const minPort = 1;
const maxPort = 65535;

// portCharacters and ipCharacters are what may not be in those boxes. They are
// dropped as they arrive, so a value that reaches the checks below is already
// made of characters that could be part of an answer.
//
// An IP box takes more than digits because an IPv6 address is written with hex
// digits and colons. What it does not take is the rest of the alphabet, so the
// box still refuses a hostname.
const portCharacters = /[^0-9]/g;
const ipCharacters = /[^0-9a-fA-F.:]/g;

// currentScreen is what is drawn. An answer that arrives after the operator has
// moved on is compared against it and dropped, so a slow call cannot draw over
// the screen that replaced the one it was made from.
let currentScreen = null;

// notice is the one line above the screen. It outlives a redraw of the same
// screen, which is how the reason a call was refused stays readable while the
// list behind it is fetched again, and it is dropped on a screen change.
let notice = null;

// refreshTimer is the timer of the status screen. It is held out here because
// what has to stop it is leaving the screen, and leaving is done from here.
// Kept inside the screen it would be started again on every visit and never
// stopped, leaving one more timer running per visit.
let refreshTimer = null;

// Redirected is what an API call throws when the answer moved the operator to
// another screen. It carries no message: the screen it lands on is the report,
// and an error line on a screen that is going away would be read as belonging
// to the new one.
class Redirected extends Error {}

// ApiError carries what the server said, word for word. The server is the only
// place that knows why a call was refused, so its wording is what is shown
// rather than a sentence made up here from the status code.
class ApiError extends Error {}

// screenName is the screen the URL asks for. The server answers every path
// under /ui/ with the same page, so the path is the only thing that says which
// screen to draw.
function screenName() {
  const rest = window.location.pathname.slice(uiPrefix.length);

  return rest === "" ? "status" : rest;
}

// screenPath is the URL a screen lives at. The status screen is /ui/ itself, so
// that the address the operator lands on first is the short one.
function screenPath(name) {
  return name === "status" ? uiPrefix : uiPrefix + name;
}

// navigate moves to a screen. The URL is changed before the screen is drawn, so
// a reload of what is on the screen comes back to the same screen: the server
// answers every /ui/ path with the page, and the page reads the path.
function navigate(name, note, replace) {
  const target = screenPath(name);

  if (replace) {
    window.history.replaceState(null, "", target);
  } else if (window.location.pathname !== target) {
    window.history.pushState(null, "", target);
  }

  notice = toNotice(note);

  showScreen(name);
}

// toNotice normalizes what a caller handed in as the line above the screen. A
// bare string is the common case and means something went wrong, so that is
// what it turns into; an answer that is worth reporting without being a failure
// says so by handing in the kind.
function toNotice(note) {
  if (note === undefined || note === null) {
    return null;
  }

  return typeof note === "string" ? { text: note, kind: "error" } : note;
}

// showScreen draws a screen by name. Every path into a screen goes through it,
// so the timer of the screen being left is stopped in one place.
function showScreen(name) {
  stopRefresh();

  const screen = screens[name];
  if (screen === undefined) {
    navigate("status", "There is no screen at " + window.location.pathname, true);

    return;
  }

  currentScreen = name;

  const enter = screen.enter === undefined ? screen.draw : screen.enter;

  run(enter);
}

// redraw draws the current screen again without entering it. It is what follows
// an action: the screen keeps the timer and the state it already has, and the
// notice the action left survives.
function redraw() {
  const screen = screens[currentScreen];
  if (screen === undefined) {
    return;
  }

  run(screen.draw);
}

// stopRefresh ends the periodic redraw of the status screen.
function stopRefresh() {
  if (refreshTimer !== null) {
    window.clearInterval(refreshTimer);
    refreshTimer = null;
  }
}

// setNotice puts a line above the screen. It is shown by the next draw, so the
// caller draws after setting it.
function setNotice(text, kind) {
  notice = { text: text, kind: kind === undefined ? "error" : kind };
}

// run carries out something that may fail and puts what went wrong on the
// screen. Every button and every draw goes through it, so no click can end as
// an unhandled rejection with a screen that silently did nothing.
function run(action) {
  Promise.resolve()
    .then(action)
    .catch(function (error) {
      if (error instanceof Redirected) {
        return;
      }

      setNotice(error.message);
      redraw();
    });
}

// render replaces everything under #app and is the only function that writes
// there. The values are set as text and never as markup, so nothing that comes
// back from the API can turn into elements.
function render(title, nodes) {
  const app = document.getElementById("app");

  app.textContent = "";

  const screen = screens[currentScreen];
  if (screen !== undefined && screen.nav) {
    app.appendChild(navigation());
  }

  const heading = document.createElement("h1");
  heading.textContent = title;
  app.appendChild(heading);

  if (notice !== null) {
    const line = document.createElement("p");
    line.className = "notice " + notice.kind;
    line.textContent = notice.text;
    app.appendChild(line);
  }

  for (const node of nodes) {
    app.appendChild(typeof node === "string" ? element("p", node) : node);
  }
}

// navigation is the bar the screens are reached from. The links carry an href
// so they can be opened in a new tab, and the click is taken over so that
// moving between screens does not fetch the page again.
function navigation() {
  const bar = document.createElement("nav");

  for (const name of Object.keys(screens)) {
    const screen = screens[name];
    if (!screen.nav) {
      continue;
    }

    const anchor = document.createElement("a");
    anchor.href = screenPath(name);
    anchor.textContent = screen.label;
    anchor.dataset.screen = name;

    if (name === currentScreen) {
      anchor.className = "current";
    }

    anchor.addEventListener("click", function (event) {
      event.preventDefault();
      navigate(name);
    });

    bar.appendChild(anchor);
  }

  const out = document.createElement("a");
  out.href = screenPath("login");
  out.className = "logout";
  out.textContent = "Log out";
  out.dataset.action = "logout";
  out.addEventListener("click", function (event) {
    event.preventDefault();
    run(logOut);
  });

  bar.appendChild(out);

  return bar;
}

// apiCall is the one door to the API. The two answers that mean the operator is
// on a screen they may not be on are turned into a move here, so that no screen
// has to repeat that check on every call it makes.
async function apiCall(method, path, body) {
  const options = {
    method: method,
    headers: { Accept: "application/json" },
    credentials: "same-origin"
  };

  // The token goes on every call and not only on the ones that change state, so
  // that nothing here has to keep a second list of which methods those are. The
  // server is the one that decides where it matters. There is none before the
  // login, which is the one call that is allowed to arrive without it.
  const token = csrfToken();
  if (token !== "") {
    options.headers[csrfHeaderName] = token;
  }

  if (body !== undefined) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }

  let response;

  try {
    response = await fetch(path, options);
  } catch (error) {
    throw new ApiError("Cannot reach the server: " + error.message);
  }

  const payload = await readPayload(response);

  if (response.status === 401 && path !== apiLoginPath) {
    navigate("login", "The session has ended. Sign in again.");

    throw new Redirected();
  }

  // The refusal that names the setup is the one that is a screen change. Other
  // 403s, if any are ever added, stay errors and are shown as they came.
  if (response.status === 403 && path !== apiSetupPath &&
      errorOf(payload, response).toLowerCase().indexOf("setup") !== -1) {
    navigate("setup", "Finish setting up the account first.", false);

    throw new Redirected();
  }

  if (!response.ok) {
    // The status rides along with the message because one screen acts on a
    // particular code: a setup that comes back as a conflict has already been
    // done, and that sends the operator to the login rather than showing a line.
    const failure = new ApiError(errorOf(payload, response));
    failure.status = response.status;

    throw failure;
  }

  return payload === null ? null : payload.data;
}

// csrfToken is the token of the session, or "" when there is not one yet. It is
// read out of the cookie at every call rather than kept in a variable, because
// the server writes the cookie again on every answer and a reload of the page
// starts with nothing held in memory.
function csrfToken() {
  for (const part of document.cookie.split(";")) {
    const pair = part.trim();

    if (pair.startsWith(csrfCookieName + "=")) {
      return decodeURIComponent(pair.slice(csrfCookieName.length + 1));
    }
  }

  return "";
}

// readPayload returns the decoded body, or null when there is none to decode.
// A body that is not JSON is not an error of its own here: what it means is
// worked out from the status code together with whatever could be read.
async function readPayload(response) {
  let text;

  try {
    text = await response.text();
  } catch (error) {
    return null;
  }

  if (text === "") {
    return null;
  }

  try {
    return JSON.parse(text);
  } catch (error) {
    return { error: text };
  }
}

// errorOf is what to show for a refused call. The API answers with an "error"
// field, but the 404 of an unrouted path comes from the framework and carries
// "message" instead, so both are read before falling back to the status code.
function errorOf(payload, response) {
  if (payload !== null) {
    if (typeof payload.error === "string" && payload.error !== "") {
      return payload.error;
    }

    if (typeof payload.message === "string" && payload.message !== "") {
      return payload.message;
    }
  }

  return "The server answered " + response.status + " " + response.statusText;
}

// element builds a node with text in it. The text is set as text, which is the
// rule everything drawn from an API answer follows.
function element(tag, text) {
  const node = document.createElement(tag);

  if (text !== undefined) {
    node.textContent = String(text);
  }

  return node;
}

// actionButton is a button that runs something. The type is set because a
// button inside a form submits it otherwise, which would send the form of the
// row the button sits next to.
//
// variant, where a caller passes one, is how a button that does something the
// operator cannot take back is told apart from the ones next to it that only
// open a form or flip a flag.
function actionButton(label, name, onClick, variant) {
  const node = document.createElement("button");

  node.type = "button";
  node.textContent = label;
  node.dataset.action = name;

  if (variant !== undefined) {
    node.className = variant;
  }

  node.addEventListener("click", function () {
    run(onClick);
  });

  return node;
}

// buildTable draws a list. A cell is either a value, which is set as text, or a
// node that was built by the caller.
//
// numericColumns names the columns that hold numbers. They are set flush right
// so that the digits of one row line up with the digits of the next, which is
// what makes a column of ports readable at a glance.
function buildTable(headers, rows, numericColumns) {
  const numeric = numericColumns === undefined ? [] : numericColumns;
  const table = document.createElement("table");
  const head = document.createElement("thead");
  const headRow = document.createElement("tr");

  headers.forEach(function (header, index) {
    const th = element("th", header);

    if (numeric.indexOf(index) !== -1) {
      th.className = "num";
    }

    headRow.appendChild(th);
  });

  head.appendChild(headRow);
  table.appendChild(head);

  const body = document.createElement("tbody");

  for (const row of rows) {
    const line = document.createElement("tr");

    row.forEach(function (cell, index) {
      const td = document.createElement("td");

      if (numeric.indexOf(index) !== -1) {
        td.className = "num";
      }

      if (cell instanceof Node) {
        td.appendChild(cell);

        // The column of buttons is as wide as its buttons and no wider, so
        // that the columns holding values keep the rest of the width. It is
        // marked here rather than by its position, because it is the last
        // column on some screens and there is none at all on others.
        if (cell.classList.contains("buttons")) {
          td.className = "actions";
        }
      } else {
        td.textContent = cell === null || cell === undefined ? "" : String(cell);
      }

      line.appendChild(td);
    });

    body.appendChild(line);
  }

  table.appendChild(body);

  // The table is handed back inside a scroller. A table of this many columns is
  // wider than a phone held upright, and without something to scroll it the
  // whole page scrolls sideways instead, taking the heading and the navigation
  // off screen with it.
  const scroller = document.createElement("div");
  scroller.className = "table-scroll";
  scroller.appendChild(table);

  return scroller;
}

// buildForm draws a form and hands the values to onSubmit. The values are read
// out of the inputs at submit time rather than tracked on every keystroke, so
// there is one place that knows what the form holds.
function buildForm(spec) {
  const form = document.createElement("form");

  form.className = "card";
  form.dataset.form = spec.name;
  form.appendChild(element("h2", spec.legend));

  const inputs = {};
  const problems = {};

  for (const field of spec.fields) {
    const row = document.createElement("div");
    row.className = "field";

    const id = spec.name + "-" + field.name;
    const label = element("label", field.label);
    label.htmlFor = id;

    const input = document.createElement("input");
    input.id = id;
    input.name = field.name;
    input.type = field.type === undefined ? "text" : field.type;
    input.dataset.field = field.name;

    if (input.type === "checkbox") {
      input.checked = Boolean(field.value);
    } else if (field.value !== undefined && field.value !== null) {
      input.value = String(field.value);
    }

    if (field.hint !== undefined) {
      input.placeholder = field.hint;
    }

    // The box stays a text box even where only digits belong in it. A number
    // box is spun by the mouse wheel while it has focus, which changes a port
    // without a keystroke, and it hands back an empty string for anything it
    // considers malformed, which leaves nothing to say what was wrong with.
    // The keypad a phone puts up is asked for separately.
    if (field.inputMode !== undefined) {
      input.setAttribute("inputmode", field.inputMode);
    }

    if (field.filter !== undefined) {
      input.addEventListener("input", function () {
        filterInput(input, field.filter);
      });
    }

    inputs[field.name] = input;

    row.appendChild(label);
    row.appendChild(input);

    // What is wrong with one value is shown under the box it was typed in. The
    // line above the screen is where a refusal of the whole call goes, and a
    // complaint about one field reads as being about all of them up there.
    const problem = element("small", "");
    problem.className = "problem";
    problem.dataset.problem = field.name;
    problem.hidden = true;

    problems[field.name] = problem;
    row.appendChild(problem);

    if (field.note !== undefined) {
      row.appendChild(element("small", field.note));
    }

    // The length is counted in bytes because that is the unit the server
    // refuses a password in, and one Hangul syllable is three of them.
    if (field.countBytes) {
      const counter = element("small", byteCountText(""));
      counter.className = "counter";

      input.addEventListener("input", function () {
        counter.textContent = byteCountText(input.value);
      });

      row.appendChild(counter);
    }

    form.appendChild(row);
  }

  const buttons = document.createElement("div");
  buttons.className = "buttons";

  const submit = document.createElement("button");
  submit.type = "submit";
  submit.textContent = spec.submitLabel;
  submit.dataset.action = spec.name + "-submit";
  buttons.appendChild(submit);

  if (spec.onCancel !== undefined) {
    buttons.appendChild(actionButton("Cancel", spec.name + "-cancel", spec.onCancel));
  }

  form.appendChild(buttons);

  form.addEventListener("submit", function (event) {
    event.preventDefault();

    const values = {};

    for (const name of Object.keys(inputs)) {
      const input = inputs[name];
      values[name] = input.type === "checkbox" ? input.checked : input.value;
    }

    // Nothing is sent while a value is one the server would refuse anyway.
    // Every field is checked rather than stopping at the first that fails, so
    // one press reports everything that has to be fixed.
    let sound = true;

    for (const field of spec.fields) {
      const message = field.check === undefined ? "" : field.check(values[field.name]);
      const problem = problems[field.name];

      problem.textContent = message;
      problem.hidden = message === "";
      inputs[field.name].classList.toggle("bad", message !== "");

      if (message !== "") {
        sound = false;
      }
    }

    if (!sound) {
      return;
    }

    run(function () {
      return spec.onSubmit(values);
    });
  });

  return form;
}

// filterInput drops what may not be in a box, as it is typed and as it is
// pasted. The caret goes back to where it was less whatever was dropped ahead
// of it, so a stray character typed in the middle of a value does not send the
// caret to the end and scatter the rest of what is being typed.
function filterInput(input, disallowed) {
  const before = input.value;
  const after = before.replace(disallowed, "");

  if (after === before) {
    return;
  }

  const caret = input.selectionStart === null ? before.length : input.selectionStart;
  const kept = before.slice(0, caret).replace(disallowed, "").length;

  input.value = after;
  input.setSelectionRange(kept, kept);
}

// checkPort says what is wrong with a port, or "" when nothing is. The box only
// takes digits, so what is left to catch is an empty one and a number outside
// what the server accepts.
function checkPort(value) {
  const trimmed = String(value).trim();

  if (trimmed === "") {
    return "Enter a port.";
  }

  const port = Number(trimmed);
  if (!Number.isInteger(port) || port < minPort || port > maxPort) {
    return "The port has to be between " + minPort + " and " + maxPort + ".";
  }

  return "";
}

// checkIP says what is wrong with an address, or "" when nothing is.
function checkIP(value) {
  const trimmed = String(value).trim();

  if (trimmed === "") {
    return "Enter an IP address.";
  }

  if (!isIPv4(trimmed) && !isIPv6(trimmed)) {
    return "This is not an IPv4 or an IPv6 address.";
  }

  return "";
}

// isIPv4 checks the dotted form. A part written with a leading zero is refused
// rather than read: 010 is eight to some software and ten to other software, so
// an address written that way does not name one host.
function isIPv4(value) {
  const parts = value.split(".");
  if (parts.length !== 4) {
    return false;
  }

  for (const part of parts) {
    if (!/^[0-9]{1,3}$/.test(part)) {
      return false;
    }

    if (part.length > 1 && part.startsWith("0")) {
      return false;
    }

    if (Number(part) > 255) {
      return false;
    }
  }

  return true;
}

// isIPv6 checks the colon form. It is not one pattern: the run that stands for
// the zero groups may sit anywhere and may appear once, and the last 32 bits
// may be written as an IPv4 address. A single pattern that covers all of that
// is longer than the rules it encodes and is read by nobody, so the address is
// cut at the run and the groups on either side are counted instead.
function isIPv6(value) {
  // A zone ("fe80::1%eth0") names an interface of the machine that wrote the
  // address down, not part of the address. The server refuses one, so does this.
  if (value.indexOf("%") !== -1) {
    return false;
  }

  const halves = value.split("::");
  if (halves.length > 2) {
    return false;
  }

  const shortened = halves.length === 2;

  let groups = splitGroups(halves[0]);
  if (shortened) {
    groups = groups.concat(splitGroups(halves[1]));
  }

  // The last group may be an IPv4 address, which fills the 32 bits of the two
  // groups it stands in for.
  let count = groups.length;
  const last = count === 0 ? "" : groups[count - 1];

  if (last.indexOf(".") !== -1) {
    if (!isIPv4(last)) {
      return false;
    }

    groups = groups.slice(0, count - 1);
    count += 1;
  }

  for (const group of groups) {
    if (!/^[0-9a-fA-F]{1,4}$/.test(group)) {
      return false;
    }
  }

  // Without the run every group is written out, so there have to be eight of
  // them. With it there has to be room left for the one group it stands for at
  // the least, which is what makes a run written where nothing is missing wrong.
  return shortened ? count <= 7 : count === 8;
}

// splitGroups cuts one side of an address into its groups. A side that is empty
// has no groups at all, which is what the end of an address that begins or ends
// with the run looks like. Splitting an empty string would hand back one empty
// group instead, and that would be counted as a group that is there.
function splitGroups(side) {
  return side === "" ? [] : side.split(":");
}

// byteCountText says how long a password is in the unit it is measured in.
function byteCountText(value) {
  return new TextEncoder().encode(value).length + " bytes (12 to 72 are accepted)";
}

// asNumber turns what was typed into a number for the API, which takes ports as
// numbers. A field that was left empty comes back as null, and the caller
// decides whether to leave it out of the request.
function asNumber(value) {
  const trimmed = String(value).trim();

  return trimmed === "" ? null : Number(trimmed);
}

// formatTime makes a timestamp readable in the time zone of the browser. The
// zero time is what the API sends for a tunnel that has never connected, and
// printing it as a date in the year one reads like a real connection at a
// strange time.
//
// The parts are put together here rather than by toLocaleString, which writes
// the date in the language the browser is set to and would put words from that
// language on a screen that is in English everywhere else.
//
// The seconds are carried. The status screen asks again every five seconds, so
// without them two rows written seconds apart read as the same moment, and a
// timestamp that just moved cannot be told from one that is stale. What keeps
// the column on one line is timeCell, not writing less into it.
function formatTime(value) {
  if (typeof value !== "string" || value === "" || value.startsWith("0001-01-01")) {
    return "never";
  }

  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return value;
  }

  return parsed.getFullYear() + "-" + pad(parsed.getMonth() + 1) + "-" + pad(parsed.getDate()) +
    " " + pad(parsed.getHours()) + ":" + pad(parsed.getMinutes()) + ":" + pad(parsed.getSeconds());
}

// timeCell is a timestamp that stays on one line. Left to wrap it breaks at the
// space between the date and the clock, and the two halves then read as two
// separate values stacked in one cell.
function timeCell(value) {
  const node = element("span", formatTime(value));

  node.className = "stamp";

  return node;
}

// pad keeps the columns of a timestamp the same width.
function pad(value) {
  return String(value).padStart(2, "0");
}

// plural is for the counts the status screen reports, so that one tunnel is not
// reported as "1 tunnels".
function plural(count, one, many) {
  return count === 1 ? one : many;
}

// showVersion puts what the server reports in the corner of every screen. It is
// asked for once, outside of any screen, and a failure is swallowed on purpose:
// the number is worth showing but nothing on the screen depends on it, and a
// line about a missing version would sit on top of the screen the operator came
// to use. An older server that does not answer this path leaves the corner
// empty rather than breaking the page.
function showVersion() {
  fetch(versionPath, { headers: { Accept: "application/json" }, credentials: "same-origin" })
    .then(function (response) {
      return response.ok ? response.json() : null;
    })
    .then(function (payload) {
      if (payload === null || typeof payload.version !== "string" || payload.version === "") {
        return;
      }

      document.getElementById("versionTag").textContent = "v" + payload.version;
    })
    .catch(function () {});
}

window.addEventListener("popstate", function () {
  notice = null;
  showScreen(screenName());
});

showVersion();
showScreen(screenName());
