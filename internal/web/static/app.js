"use strict";

// Where the UI is served from. The part of the path after it names the screen.
const uiPrefix = "/ui/";

// screenName is the screen the URL asks for. The server answers every path
// under /ui/ with this page, so the path is the only thing that says which
// screen to draw. No screen is built yet; this is the one place that reads it.
function screenName() {
  const rest = window.location.pathname.slice(uiPrefix.length);

  return rest === "" ? "status" : rest;
}

// render replaces everything under #app. Every screen goes through it, so the
// page is written in one place. The values are set as text and never as
// markup, so what comes back from the API cannot turn into elements.
function render(title, lines) {
  const app = document.getElementById("app");

  app.textContent = "";

  const heading = document.createElement("h1");
  heading.textContent = title;
  app.appendChild(heading);

  for (const line of lines) {
    const paragraph = document.createElement("pre");
    paragraph.textContent = line;
    app.appendChild(paragraph);
  }
}

// loadStatus asks the API for the tunnel state. It is the one call that shows
// the plumbing works end to end: the page came from the binary and the data
// comes from the API, which is behind the session while this page is not.
async function loadStatus() {
  let response;

  try {
    response = await fetch("/api/status", {
      headers: { Accept: "application/json" },
      credentials: "same-origin"
    });
  } catch (error) {
    render("Tunnel Manager", ["Cannot reach the API: " + error.message]);

    return;
  }

  // Until there is a session the API answers this way, and the login screen
  // is what goes here.
  if (response.status === 401) {
    render("Tunnel Manager", [
      "Login required.",
      "Screen asked for: " + screenName()
    ]);

    return;
  }

  const body = await response.text();

  render("Tunnel Manager", [
    "Screen asked for: " + screenName(),
    "GET /api/status -> " + response.status,
    body
  ]);
}

loadStatus();
