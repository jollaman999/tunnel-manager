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
function actionButton(label, name, onClick) {
  const node = document.createElement("button");

  node.type = "button";
  node.textContent = label;
  node.dataset.action = name;
  node.addEventListener("click", function () {
    run(onClick);
  });

  return node;
}

// buildTable draws a list. A cell is either a value, which is set as text, or a
// node that was built by the caller.
function buildTable(headers, rows) {
  const table = document.createElement("table");
  const head = document.createElement("thead");
  const headRow = document.createElement("tr");

  for (const header of headers) {
    headRow.appendChild(element("th", header));
  }

  head.appendChild(headRow);
  table.appendChild(head);

  const body = document.createElement("tbody");

  for (const row of rows) {
    const line = document.createElement("tr");

    for (const cell of row) {
      const td = document.createElement("td");

      if (cell instanceof Node) {
        td.appendChild(cell);
      } else {
        td.textContent = cell === null || cell === undefined ? "" : String(cell);
      }

      line.appendChild(td);
    }

    body.appendChild(line);
  }

  table.appendChild(body);

  return table;
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

    inputs[field.name] = input;

    row.appendChild(label);
    row.appendChild(input);

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

    run(function () {
      return spec.onSubmit(values);
    });
  });

  return form;
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

// pad keeps the columns of a timestamp the same width.
function pad(value) {
  return String(value).padStart(2, "0");
}

// plural is for the counts the status screen reports, so that one tunnel is not
// reported as "1 tunnels".
function plural(count, one, many) {
  return count === 1 ? one : many;
}

window.addEventListener("popstate", function () {
  notice = null;
  showScreen(screenName());
});

showScreen(screenName());
