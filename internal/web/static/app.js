"use strict";

// Where the UI is served from. The part of the path after it names the screen.
const uiPrefix = "/ui/";

// statusRefreshMs is how often the status screen asks again. It is the default
// period of the reconcile loop, so what is on the screen is never more than one
// pass of that loop behind what the server has done.
const statusRefreshMs = 5000;

// scrollQuietMs is how long after the last scroll a refresh of the screen
// waits.
//
// A draw replaces everything under #app. render puts the position back, which
// covers a page that is standing still, but a phone scrolls on after the finger
// has left and a glide that is handed a new set of elements halfway through
// does not carry on gliding: it stops where it was interrupted, which is not
// where the reader was going.
//
// What the wait has to cover is the gap between two scroll events inside one
// movement and not the movement itself: they arrive about one a frame while the
// page is moving and stop arriving when it settles, so a few frames of quiet
// already tell the end of a scroll from the middle of one. 400ms is many frames
// past that and is under a tenth of the period between two refreshes, so a
// refresh that waits for it is not meaningfully later than one that did not.
const scrollQuietMs = 400;

// apiLoginPath and apiSetupPath are the two calls whose refusals must not be
// turned into a move to another screen. A 401 from the login is what wrong
// credentials look like, and a 403 from the setup would send the operator to
// the screen they are already on.
const apiLoginPath = "/api/login";
const apiSetupPath = "/api/setup";

// setupRequiredCode is the refusal that sends the operator to the setup screen.
// It is the name the server raises it under, which is what the move is decided
// on: the sentence beside it is drawn in the language of the page and is not
// something to read a decision out of.
const setupRequiredCode = "auth.setup.required";

// apiUninstallPath is the third call whose 401 means something else. It is the
// password box under the uninstall being wrong, and the session it was sent
// with is still good, so sending the operator to the login would both lose what
// they were doing and tell them something that is not so.
const apiUninstallPath = "/api/uninstall";

// apiAccountPath is the fourth, and for the same reason: a 401 from it is the
// current password box being wrong. The session that sent it is not only still
// good, it is the one session the change keeps, so a move to the login would be
// saying the opposite of what happened.
const apiAccountPath = "/api/account";

// These are the 401s that do not mean the session is over. Each of them is a
// password box in front of the operator being wrong, and they are read by the
// name the server raises them under rather than by the path they came from.
//
// The host key approval is the one with no path to compare against: the
// approval of one host is POST /api/host/<id>/host-key, so there is no single
// string, and the same refusal also comes back from the call that approves many
// at once. Emptying the log has a path of its own, but the read of the log is
// that same path with a query on the end, so a comparison there would be
// resting on the query being what tells the two calls apart. The name the
// server raises the refusal under is read instead.
//
// Each of them means what the uninstall and the account 401s mean: the password
// box in front of the operator was wrong. The session that carried it is
// untouched and the server changed nothing, so the operator stays on the screen
// they were on and the panel says why, the way every other refusal of that call
// is shown.
//
// They are two constants and not one list of strings because the test that
// holds these names to the ones the server raises reads them as constants. A
// list would leave that test with nothing to read and the check would go
// quiet without failing.
const hostKeyPasswordWrongCode = "host.host_key.password_wrong";
const logsClearPasswordWrongCode = "logs.clear.password_wrong";
const updatePasswordWrongCode = "update.password_wrong";

const passwordWrongCodes = [
  hostKeyPasswordWrongCode,
  logsClearPasswordWrongCode,
  updatePasswordWrongCode
];

// csrfCookieName and csrfHeaderName are the two ends of the CSRF check. The
// server hands the token of the session out in a cookie it leaves readable from
// here on purpose, and wants it back in a header, which a page on another
// origin cannot put on a request to this one without a preflight this server
// never answers.
const csrfCookieName = "tm_csrf";
const csrfHeaderName = "X-CSRF-Token";

// themeKey is where the theme the operator picked is kept, and index.html holds
// the same string: it is read there, in the head, so the first paint is already
// in the right colour. It is a key of the local storage of the one browser and
// is never sent anywhere. Which theme a screen is read in belongs to the screen
// it is read on and not to the account it is read with, and a browser that
// refuses to keep it loses nothing but the pick.
const themeKey = "tm_theme";

// langKey is where the language the operator picked is kept, and index.html
// holds the same string for the same reason the theme key is held there: lang
// and dir go on <html> before the first paint. It is a key of the local storage
// of the one browser and is never sent anywhere.
//
// The pick belongs to the browser and not to the account: the login screen has
// no session to read a setting out of, so a language that lived on the server
// could not be honoured on the one screen every operator starts at.
const langKey = "tm_lang";

// languages is what can be picked, in the order a browser language is matched
// against them and in the order they are offered.
//
// The name is what the language calls itself and not what English calls it. A
// reader looking for their own language is looking for the word they would
// write it with; a list of English names is a list they cannot search.
//
// index.html holds the codes and the right-to-left one again, because the head
// script settles the language before this file has been fetched. A test
// compares the two lists, so a language added here and not there is a failed
// build rather than a page that quietly ignores the new code.
const languages = [
  { code: "en", name: "English", rtl: false },
  { code: "ko", name: "한국어", rtl: false },
  { code: "ja", name: "日本語", rtl: false },
  { code: "zh", name: "中文", rtl: false },
  { code: "es", name: "Español", rtl: false },
  { code: "fr", name: "Français", rtl: false },
  { code: "de", name: "Deutsch", rtl: false },
  { code: "pt-BR", name: "Português (Brasil)", rtl: false },
  { code: "ru", name: "Русский", rtl: false },
  { code: "ar", name: "العربية", rtl: true },
  { code: "hi", name: "हिन्दी", rtl: false },
  { code: "vi", name: "Tiếng Việt", rtl: false },
  { code: "th", name: "ไทย", rtl: false }
];

// baseLang is the language every catalog falls back to, key by key. It is
// fetched whatever else is, so a key that has not been translated yet is a word
// in English on the screen and never a blank or the name of the key.
const baseLang = "en";

// langPath is where a catalog is fetched from. The files are served out of the
// same binary as this one, so a UI on a host that reaches nothing else still
// has all thirteen languages.
const langPath = "/ui/lang/";

// placeholder is what a value is written into a sentence by. The name inside
// the braces and not the order is what says which value goes where, so a
// language that puts the count before the noun and one that puts it after can
// be the same key with the braces in different places.
//
// An underscore is part of a name because the values the server hands over are
// named by the server: the arguments of a refusal and the fields of a log line
// arrive under the names they are written under in Go, which are snake_case. A
// sentence that renamed them would be a second list to keep in step.
const placeholder = /\{([a-z][a-z0-9_]*)\}/g;

// versionPath is where the number in the corner is read from. It is served from
// under /ui/ rather than from /api/, so the login screen, which has no session
// yet, can show it too.
const versionPath = "/ui/version.json";

// The bounds the API takes a port in. The server refuses anything outside them,
// and the same bounds are checked here so that a typo is reported next to the
// box it was typed in instead of after a round trip.
const minPort = 1;
const maxPort = 65535;

// The bounds a password is held to, in bytes. They are the ones
// internal/api/auth.go holds the account to, and the password that seals an
// exported file is held to the same: the file carries the SSH credentials of
// every Host and is kept wherever it is put, so it stands to be guessed at for
// longer than a login does.
//
// The unit is bytes and not characters because that is what the server counts.
// One Hangul syllable is three of them.
const minPasswordBytes = 8;
const maxPasswordBytes = 72;

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

// texts is the catalog of the language the page is in and baseTexts is the
// English one under it. Both are null until they have been fetched, and that
// null is what tells the two switches in the corner to stay unlabelled: a
// control that showed an English word for a moment on a page asked for in
// Korean is the flip the head script of index.html exists to avoid.
//
// A fetch that fails leaves an empty table rather than null. The screens have
// to be drawn either way, and what is lost is the words on a few controls; left
// null the page would wait on a catalog that is never coming and draw nothing
// at all.
let texts = null;
let baseTexts = null;

// installationLang is the language this installation was set up to draw a
// browser that has picked none of its own in. It is null until it has been
// asked for and where the answer could not be had, and "" where the
// installation names no language, which is the setting saying that every
// browser is shown what it asks for.
//
// It is kept here rather than read at every draw because it comes from a call
// that needs a session: the login screen cannot make it at all, and the screens
// behind the login would each be making it again.
let installationLang = null;

// loadedVersion is the number the corner shows, kept because the corner is
// drawn again whenever the language changes and one fetch is enough for all of
// them.
let loadedVersion = null;

// notice is the one line above the screen. It outlives a redraw of the same
// screen, which is how the reason a call was refused stays readable while the
// list behind it is fetched again, and it is dropped on a screen change.
let notice = null;

// refreshTimer is the timer of the status screen. It is held out here because
// what has to stop it is leaving the screen, and leaving is done from here.
// Kept inside the screen it would be started again on every visit and never
// stopped, leaving one more timer running per visit.
let refreshTimer = null;

// scrolledAt is when the page last moved, and scrollRetryTimer is the refresh
// that is waiting for it to stop moving. The timer is here beside refreshTimer
// and for the same reason: leaving the screen has to be able to stop it, and
// leaving is done from here.
let scrolledAt = 0;
let scrollRetryTimer = null;

// periodicDraw says the draw being built is a tick of the refresh rather than
// something the operator asked for, and heldScreen is one such draw that is
// finished and waiting for the page to stop moving.
//
// Waiting before the request is not enough on its own. A tick is let through
// while the page is still, the answer takes a round trip to come back, and the
// screen is replaced when it arrives: a finger that comes down in between meets
// the swap anyway. What the reader sees is the list jumping under them, which
// is the thing the waiting was for.
//
// So the draw is built either way and held at the last step, which is also the
// only step that costs the reader anything. Nothing is fetched twice, and the
// screen goes up the moment the page settles.
let periodicDraw = false;
let heldScreen = null;
let heldScreenTimer = null;

// pointerDown is whether a finger, a pen or a mouse button is held on the page
// right now. It is counted apart from the scroll because the two are not the
// same thing: a pointer can be down for a while before the page moves, and that
// is the whole of the time the reader is about to drag it.
//
// A mouse counts and not only a finger. A held mouse button over a row is a
// drag of an inner scroller or a selection being made, and both are undone by a
// screen that is rebuilt underneath them; a reader holding one is as much in
// the middle of something as a reader with a finger down.
let pointerDown = false;

// drawnScreen is the screen the last render drew. It is what tells a refresh of
// the screen that is up from the first draw of one that has just been moved to,
// which are the two cases the scroll position is treated differently in.
let drawnScreen = null;

// modalStack is the panels that are over the screen, oldest first. It is a
// stack and not one panel because what has to stay true is that the page
// behind is locked while any of them is up and is let go when the last one
// leaves, and a count of one cannot say that of the second panel a panel
// opened.
//
// It is also what Esc reads: the key closes the panel on top and nothing else,
// so a panel that opened another is not taken down with it.
const modalStack = [];

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
    navigate("status", t("app.no-screen.error", { path: window.location.pathname }), true);

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

// stopRefresh ends the periodic redraw of the status screen, and with it the
// tick that was waiting for the scrolling to stop. A tick left waiting would
// come due on the screen that replaced the one it was started on.
function stopRefresh() {
  if (refreshTimer !== null) {
    window.clearInterval(refreshTimer);
    refreshTimer = null;
  }

  if (scrollRetryTimer !== null) {
    window.clearTimeout(scrollRetryTimer);
    scrollRetryTimer = null;
  }

  dropHeldScreen();
}

// dropHeldScreen forgets a draw that was waiting to go up. What waits belongs
// to the screen being left, and putting it up afterwards would draw it over the
// one that replaced it.
function dropHeldScreen() {
  if (heldScreenTimer !== null) {
    window.clearTimeout(heldScreenTimer);
    heldScreenTimer = null;
  }

  heldScreen = null;
}

// putUpHeldScreen waits out the scrolling and then draws what was held. It asks
// again rather than trusting one wait, because a reader who is still moving
// when it looks is a reader it has to keep waiting for.
function putUpHeldScreen() {
  heldScreenTimer = window.setTimeout(function () {
    heldScreenTimer = null;

    if (heldScreen === null) {
      return;
    }

    if (pageIsHeld()) {
      putUpHeldScreen();

      return;
    }

    const held = heldScreen;

    heldScreen = null;

    // The screen it was drawn for may not be the one that is up any more.
    if (held.screen === currentScreen) {
      paint(held.title, held.nodes);
    }
  }, scrollQuietMs);
}

// pageIsHeld reports whether the reader has hold of the page at this moment.
//
// A finger resting on the screen counts, and it has to. Watching the scroll
// alone watches the effect and not the cause: a finger that is down but has not
// moved yet fires no scroll event, so the page reads as still, the screen is
// replaced under the hand that is about to drag it, and the drag begins on
// something that was rebuilt a moment ago. What was reported was exactly that,
// a refresh while touching rather than while scrolling.
//
// Text that has been dragged over counts too, and for longer than the drag.
// Selecting the sentence under an error is how it gets copied into a search or
// a message, and a draw that replaces the screen drops the selection: the words
// are still there and the highlight is not, so the copy takes nothing. The hold
// lasts as long as the selection does, which the next click anywhere ends.
function pageIsHeld() {
  return pointerDown || textIsSelected() || Date.now() - scrolledAt < scrollQuietMs;
}

// textIsSelected is whether the reader has some of the screen highlighted.
//
// Only a selection inside the screen counts. One in the address bar or in
// another frame is not something a draw of this page would take away, and
// getSelection is allowed to answer with nothing at all, which is read here as
// nothing selected rather than as a reason to fail a draw.
function textIsSelected() {
  const app = document.getElementById("app");

  if (app === null || window.getSelection === undefined) {
    return false;
  }

  const picked = window.getSelection();

  if (picked === null || picked.isCollapsed || picked.rangeCount === 0) {
    return false;
  }

  return app.contains(picked.getRangeAt(0).commonAncestorContainer);
}

// refreshWhenStill takes a tick of the periodic refresh, once the page has
// stopped moving.
//
// A tick that lands while it is moving is not dropped. It is held and taken as
// soon as the scrolling stops, so a screen cannot be left standing on an old
// answer because a finger happened to be down when the timer went off. Only one
// tick is ever held: the ones behind it would fetch the same answer it does.
function refreshWhenStill(draw) {
  if (scrollRetryTimer !== null) {
    return;
  }

  if (!pageIsHeld()) {
    periodicDraw = true;

    const started = draw();

    if (started !== undefined && typeof started.finally === "function") {
      // run takes something to call, not something already running. Handed the
      // promise itself it would be passed to then as a value, which then
      // ignores, and a draw that failed would go to nobody.
      run(function () {
        return started.finally(function () {
          periodicDraw = false;
        });
      });
    } else {
      periodicDraw = false;
    }

    return;
  }

  scrollRetryTimer = window.setTimeout(function () {
    scrollRetryTimer = null;

    refreshWhenStill(draw);
  }, scrollQuietMs);
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
  // A tick of the refresh that came back while the reader is moving is held
  // rather than put up. It is the swap itself that costs them, not the fetch,
  // so holding it here is the last place it can be stopped and the only one
  // that matters. A draw the operator asked for goes up whatever the page is
  // doing: they are waiting for it.
  if (periodicDraw && pageIsHeld()) {
    heldScreen = { title: title, nodes: nodes, screen: currentScreen };

    if (heldScreenTimer === null) {
      putUpHeldScreen();
    }

    return;
  }

  // Anything that is drawn now replaces whatever was waiting to be.
  dropHeldScreen();

  paint(title, nodes);
}

// paint puts a screen up. It is what render does once it has decided that now
// is the moment, and what the held draw does when its moment comes.
function paint(title, nodes) {
  const app = document.getElementById("app");

  // Where the page is being read, taken before the screen goes.
  //
  // Emptying #app takes the height of the page down to the height of the
  // heading for as long as it takes to build the screen again, and a page that
  // is suddenly shorter than the position it is scrolled to is scrolled back to
  // the top by the browser. Without this, every refresh of the status screen
  // takes the reader back to the first row.
  //
  // It is put back only where the screen is the one that was up. A move to
  // another screen starts at the top, which is where it would have started
  // before any of this.
  const atX = window.scrollX;
  const atY = window.scrollY;
  const sameScreen = drawnScreen === currentScreen;

  // Where each box that scrolls on its own is scrolled to, taken before the
  // screen goes.
  //
  // The page position above covers the page and nothing else. A wide table is
  // in a scroller of its own, and the message under a failing tunnel is read by
  // dragging that scroller sideways; the draw builds a new scroller, which
  // starts at its beginning, and what the reader had dragged into view slid
  // back off the screen. It read as the screen refreshing under them, which it
  // was.
  //
  // They are matched by the order they come in and not by a name, because they
  // have none: what is being put back is the second scroller of this screen
  // onto the second scroller of the same screen, which is the same box drawn
  // again.
  const scrolledTo = insideScrolls(app);

  app.textContent = "";

  const screen = screens[currentScreen];
  if (screen !== undefined && screen.nav) {
    app.appendChild(navigation());
  }

  const heading = document.createElement("h1");
  heading.textContent = title;

  // On a screen with no navigation there is no name above the heading, so the
  // two switches go on the heading's own row instead.
  app.appendChild(screen !== undefined && screen.nav ? heading : topLine(heading));

  if (notice !== null) {
    const line = document.createElement("p");
    line.className = "notice " + notice.kind;
    line.textContent = notice.text;
    app.appendChild(line);
  }

  for (const node of nodes) {
    app.appendChild(typeof node === "string" ? element("p", node) : node);
  }

  drawnScreen = currentScreen;

  if (sameScreen) {
    window.scrollTo(atX, atY);

    const boxes = insideScrolls(app);

    for (let at = 0; at < boxes.length && at < scrolledTo.length; at += 1) {
      boxes[at].node.scrollLeft = scrolledTo[at].left;
      boxes[at].node.scrollTop = scrolledTo[at].top;
    }
  } else {
    window.scrollTo(0, 0);
  }
}

// insideScrolls is every box on the screen that scrolls inside itself, in the
// order they are drawn, with where each one is scrolled to.
//
// The list is of the boxes that are given a scrollbar of their own by the
// stylesheet. A box that is not scrolled anywhere is still in it: the count and
// the order are what the two lists are matched on, and leaving the still ones
// out would shift every one after them onto the wrong box.
function insideScrolls(app) {
  const found = [];

  for (const node of app.querySelectorAll(".table-scroll, .topology, pre")) {
    found.push({ node: node, left: node.scrollLeft, top: node.scrollTop });
  }

  return found;
}

// cornerControls is the box holding the language picker and the theme switch.
//
// It is held here rather than looked up when it is wanted. It is built in
// index.html and then moved into whichever row is being drawn, which puts it
// inside #app; emptying #app to draw the next screen takes it out of the
// document, and a lookup by id after that finds nothing. Holding the element
// keeps it alive between screens: it is out of the document for the moment a
// screen is being built and back in it before the frame is painted.
const cornerControls = document.getElementById("cornerControls");

// topLine puts the language picker and the theme switch on the end of the row
// that carries the name of the page.
//
// The two are moved into the row rather than copied into it. They are built
// once in index.html, before any script has run, and carry the listeners set on
// them there and then; appending an element that is already somewhere else
// moves it, with everything hung on it, so there is one of each for the life of
// the tab however many times the screen is drawn.
//
// They sit in the flow and are not fixed to the corner of the window. Fixed,
// they stayed over the page while it scrolled and sat on top of whatever was
// under them; here they go up with the name they belong to.
function topLine(node) {
  const row = document.createElement("div");

  row.className = "topline";
  row.appendChild(node);

  if (cornerControls !== null) {
    row.appendChild(cornerControls);
  }

  return row;
}

// navigation is the top of every screen: the product name on one row and the
// links the screens are reached from on the next. The links carry an href so
// they can be opened in a new tab, and the click is taken over so that moving
// between screens does not fetch the page again.
//
// The two are separate rows because they do separate things. The name is the
// same on every screen, and the links are how one of them is chosen; on one row
// the name reads as the first of the places to go.
function navigation() {
  const top = document.createElement("div");
  top.className = "topbar";

  // The product name sits here rather than in the heading, because the heading
  // says which screen this is. Without it the name is only ever seen on the way
  // in, and a tab left open says nothing about what it belongs to.
  const brand = document.createElement("div");
  brand.className = "brand";
  brand.textContent = t("common.brand.text");
  top.appendChild(topLine(brand));

  const bar = document.createElement("nav");

  for (const name of Object.keys(screens)) {
    const screen = screens[name];
    if (!screen.nav) {
      continue;
    }

    const anchor = document.createElement("a");
    anchor.href = screenPath(name);
    anchor.textContent = t(screen.label);
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
  out.textContent = t("nav.logout.link");
  out.dataset.action = "logout";
  out.addEventListener("click", function (event) {
    event.preventDefault();
    run(logOut);
  });

  bar.appendChild(out);

  top.appendChild(bar);

  return top;
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
    throw new ApiError(t("api.unreachable.error", { reason: error.message }));
  }

  const payload = await readPayload(response);

  if (response.status === 401 && path !== apiLoginPath && path !== apiUninstallPath &&
      path !== apiAccountPath && !saysPasswordWrong(payload)) {
    // The session is gone, so the login screen that follows is drawn for a
    // reader with no session, in the language the browser asks for and not
    // the one the installation names. The notice is picked after the switch
    // so that it comes in the same language as the screen it sits on.
    await forgetInstallationLang();
    navigate("login", t("api.session-ended.error"));

    throw new Redirected();
  }

  // The refusal that names the setup is the one that is a screen change. Other
  // 403s, if any are ever added, stay errors and are shown as they came.
  if (response.status === 403 && path !== apiSetupPath && saysSetupFirst(payload, response)) {
    navigate("setup", t("api.setup-first.error"), false);

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

// saysPasswordWrong reads whether a 401 is a password box in front of the
// operator being wrong rather than the session being over.
//
// Only the code decides it. The sentence beside it is drawn in the language of
// the page, and a 401 that carries no code at all is what an ended session
// looks like, so anything this does not find the name in stays a move to the
// login: a session that really is gone must not be mistaken for a typo.
function saysPasswordWrong(payload) {
  if (payload === null || typeof payload.error_code !== "string") {
    return false;
  }

  return passwordWrongCodes.indexOf(payload.error_code) !== -1;
}

// saysSetupFirst reads whether a refusal is the account setup not being
// finished, which is the one refusal that moves the operator to another screen.
//
// The code is what decides it. The sentence was what decided it before, and a
// sentence is the one thing about an answer that changes with the language the
// page is in: read in Korean it holds no word "setup", so a screen that matched
// on the word would stay where it was and show the refusal over and over.
//
// The word is still matched where the answer carries no code at all, which is
// what a server from before the refusals were named answers with.
function saysSetupFirst(payload, response) {
  if (payload !== null && typeof payload.error_code === "string" && payload.error_code !== "") {
    return payload.error_code === setupRequiredCode;
  }

  return errorOf(payload, response).toLowerCase().indexOf("setup") !== -1;
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
//
// The sentence this UI knows how to say itself comes first. What the server
// sends is English whatever language the page is in, so a refusal that names
// itself is said here instead, in the words of the page.
function errorOf(payload, response) {
  if (payload !== null) {
    const said = refusalText(payload);

    if (said !== null) {
      return said;
    }

    if (typeof payload.error === "string" && payload.error !== "") {
      return payload.error;
    }

    if (typeof payload.message === "string" && payload.message !== "") {
      return payload.message;
    }
  }

  return t("api.status.error", { status: response.status, said: response.statusText });
}

// refusalKey is where the sentence of a refusal is kept. The server names every
// answer that says no, and the name is the key of the sentence with the role
// every refusal carries at the end of it.
function refusalKey(code) {
  return "error." + code + ".error";
}

// refusalText is what a named refusal reads as in the language of the page, or
// null where there is nothing to say it with.
//
// Null is the answer for a code no catalog has heard of, which is what an older
// screen meets from a newer server, and the caller shows the English sentence
// the answer carries beside the code. That sentence is always there, so a
// refusal this UI does not know is still a refusal the operator can read.
//
// The values are the ones the server wrote into its own sentence, handed over
// by name. They are written in by t, which puts them through the same one pass
// as any other value, and everything t hands back is put on the page as text.
function refusalText(payload) {
  if (typeof payload.error_code !== "string" || payload.error_code === "") {
    return null;
  }

  const key = refusalKey(payload.error_code);

  if (entry(texts, key) === null && entry(baseTexts, key) === null) {
    return null;
  }

  const values = payload.error_args;

  return t(key, namedValues(values !== null && typeof values === "object" ? values : {}));
}

// codeSuffix is what the server puts on the end of the name of a value, or of a
// field, to carry the code of the string beside it: "what" and "what_code".
const codeSuffix = "_code";

// namedValues is the values of a refusal with the ones the server named said in
// the language of the page.
//
// A value that is itself a phrase of the server's, the name of a kind of file
// say, arrives twice: as the English written into the sentence, under the name
// the sentence asks for, and as a code under that name with "_code" on the end.
// The phrase behind the code is looked up the way any named string is, and is
// written with the same values, so a phrase that asks for one of them by name
// finds it. Where no catalog knows the code the English stays, which is what a
// refusal from a newer server reads as on this screen.
function namedValues(values) {
  const said = {};

  for (const name of Object.keys(values)) {
    said[name] = values[name];
  }

  for (const name of Object.keys(values)) {
    if (!name.endsWith(codeSuffix)) {
      continue;
    }

    const of = name.slice(0, -codeSuffix.length);

    if (Object.prototype.hasOwnProperty.call(values, of)) {
      said[of] = serverText(values[of], values[name], values);
    }
  }

  return said;
}

// serverTextKey is where a string the server names is kept: the sentence an
// answer carries beside its English, or the name of a thing the screen puts in
// a cell of its own. The answer carries the code next to the English, under
// the name of the English field with "_code" on the end.
function serverTextKey(code) {
  return "answer." + code + ".text";
}

// serverText is what a named string of the server reads as in the language of
// the page. It is the English the answer carries where the answer names
// nothing, which a server from before the codes does not, and where no catalog
// has heard of the code, which is what this screen meets from a newer server.
// The values are written in by t, as the values of a refusal are.
function serverText(english, code, values) {
  if (typeof code !== "string" || code === "") {
    return english;
  }

  const key = serverTextKey(code);

  if (entry(texts, key) === null && entry(baseTexts, key) === null) {
    return english;
  }

  return t(key, values !== null && typeof values === "object" ? values : {});
}

// logIdField is the field a log line carries its identifier under, and
// logLineKey is where the sentence of that identifier is kept. The file stays
// English so that it can be grepped and sent with a support request; the line
// on the screen is drawn from the identifier instead of from the words.
const logIdField = "log_id";

function logLineKey(id) {
  return "log." + id + ".text";
}

// logFields is what a line carried besides the four the screen gives a column
// of its own. The server hands them over as the line was written: a line the
// JSON encoder wrote comes as an object, and anything else is not read here.
function logFields(line) {
  if (typeof line.extra !== "string" || line.extra === "") {
    return null;
  }

  let held;

  try {
    held = JSON.parse(line.extra);
  } catch (error) {
    return null;
  }

  return held !== null && typeof held === "object" && !Array.isArray(held) ? held : null;
}

// logLineText is what one line of the log reads as in the language of the page.
//
// What is shown where the line names no sentence is the message out of the
// file, in the English it was written in. A log file holds the lines of every
// version that ever wrote to it, and the ones written before the lines were
// named carry no identifier at all; so does a line of something else that ended
// up in the file. None of those is an error to report: the words are there to
// be read, and this screen is the one place they can be read from.
function logLineText(line) {
  const message = line.parsed ? line.message : line.raw;
  const fields = logFields(line);

  if (fields === null) {
    return message;
  }

  const id = fields[logIdField];

  if (typeof id !== "string" || id === "") {
    return message;
  }

  const key = logLineKey(id);

  if (entry(texts, key) === null && entry(baseTexts, key) === null) {
    return message;
  }

  // The values are the fields of the line itself, under the names the line
  // wrote them under, so a sentence that needs one names it and a language that
  // wants it somewhere else moves the braces.
  return t(key, fields);
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
// variant, where a caller passes one, is the class that paints the button: it
// is how the one press a card is there for, or one that the operator cannot
// take back, is told apart from the ones next to it that only open a form or
// flip a flag.
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
    // A row may carry something that belongs under it rather than in it. It is
    // given the whole width, because what goes there is a sentence and a
    // sentence in a column of a table this wide is a column of single words.
    const under = row.under === undefined ? null : row.under;
    const cells = under === null ? row : row.cells;

    const line = document.createElement("tr");

    cells.forEach(function (cell, index) {
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

    if (under !== null) {
      const detail = document.createElement("tr");
      const cell = document.createElement("td");

      detail.className = "under";
      cell.setAttribute("colspan", String(headers.length));
      cell.appendChild(under);
      detail.appendChild(cell);
      body.appendChild(detail);
    }
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

// bulletList is a list of sentences. It is a list and not one paragraph with
// commas in it because what it is used for is a set of things that are each
// gone or not gone on their own, and a reader counting them has to be able to.
function bulletList(items) {
  const list = document.createElement("ul");

  for (const item of items) {
    list.appendChild(element("li", item));
  }

  return list;
}

// textControl is the box a value is typed into.
//
// It stays a text box even where only digits belong in it. A number box is spun
// by the mouse wheel while it has focus, which changes a port without a
// keystroke, and it hands back an empty string for anything it considers
// malformed, which leaves nothing to say what was wrong with. The keypad a
// phone puts up is asked for separately.
function textControl(field) {
  // A PEM block is the one value here that is many lines long. It goes in a
  // textarea rather than in a box, because a box shows one line of a file that
  // is thirty and gives nothing to check a paste against. The value is read the
  // same way a box is read, since a textarea carries one too.
  if (field.type === "textarea") {
    const area = document.createElement("textarea");

    area.rows = field.rows === undefined ? 8 : field.rows;

    if (field.value !== undefined && field.value !== null) {
      area.value = String(field.value);
    }

    if (field.hint !== undefined) {
      area.placeholder = field.hint;
    }

    return area;
  }

  const input = document.createElement("input");

  input.type = field.type === undefined ? "text" : field.type;

  if (input.type === "checkbox") {
    input.checked = Boolean(field.value);
  } else if (field.value !== undefined && field.value !== null) {
    input.value = String(field.value);
  }

  if (field.hint !== undefined) {
    input.placeholder = field.hint;
  }

  if (field.inputMode !== undefined) {
    input.setAttribute("inputmode", field.inputMode);
  }

  if (field.filter !== undefined) {
    input.addEventListener("input", function () {
      filterInput(input, field.filter);
    });
  }

  return input;
}

// keyFileLimit is the largest file the drop area below reads. A private key in
// PEM is a few kilobytes: an RSA 4096 key, the largest in common use, is about
// 3.2 KB on disk, and one that is protected by a passphrase is a little larger
// again. 64 KiB is far above every key there is and small enough that a file
// dropped by mistake, an image or a log, is refused before the browser reads it
// into the page.
const keyFileLimit = 64 * 1024;

// dropArea is the patch of the form a key file is dropped onto. What is read
// goes into the box beside it and nowhere else: the file is read here, in the
// browser, with FileReader, and what is sent is the text of the key, the same
// as if it had been pasted. Nothing uploads a file.
//
// It sits next to the box rather than replacing it, because the two are for
// different situations: the key file is on the machine the browser runs on and
// can be dragged in, or it is in a terminal somewhere and gets pasted.
//
// what, ever and limit are how the two sentences below and the size it refuses
// are said for something other than a key: an exported configuration is dropped
// onto one of these too, and it is a file of another size that is named another
// way. Left out, they are the key file this was first written for.
function dropArea(input, spec) {
  const what = spec.what === undefined ? t("form.drop-key.text") : spec.what;
  const ever = spec.ever === undefined ? t("form.drop-key-ever.text") : spec.ever;
  const limit = spec.limit === undefined ? keyFileLimit : spec.limit;
  const then = spec.then === undefined ? t("form.drop-then-save.text") : spec.then;

  const zone = document.createElement("div");

  zone.className = "drop-zone";
  zone.dataset.drop = input.name;
  zone.appendChild(element("span", spec.label));

  // What went wrong with a file, and which file was read, are both said here,
  // under the area the file was let go over.
  const said = element("small", "");
  said.className = "drop-said";
  said.hidden = true;
  zone.appendChild(said);

  function say(message, bad) {
    said.textContent = message;
    said.hidden = false;
    said.classList.toggle("problem", bad === true);
  }

  // A drag is only a drop if the default is prevented on the way in. The class
  // is what says so on the screen: without it the operator is dragging a file
  // over a page with nothing to tell them it will be taken.
  function over(event) {
    event.preventDefault();
    zone.classList.add("drop-over");
  }

  zone.addEventListener("dragenter", over);
  zone.addEventListener("dragover", over);
  zone.addEventListener("dragleave", function () {
    zone.classList.remove("drop-over");
  });

  zone.addEventListener("drop", function (event) {
    event.preventDefault();
    zone.classList.remove("drop-over");

    const transfer = event.dataTransfer;
    const files = transfer === null || transfer === undefined ? null : transfer.files;

    if (files === null || files === undefined || files.length === 0) {
      say(t("form.drop-not-file.error", { what: what }), true);

      return;
    }

    const file = files[0];

    if (file.size > limit) {
      say(t("form.drop-too-large.error",
        { name: file.name, size: file.size, ever: ever, limit: limit }), true);

      return;
    }

    const reader = new FileReader();

    reader.onerror = function () {
      say(t("form.drop-unreadable.error", { name: file.name }), true);
    };

    reader.onload = function () {
      input.value = String(reader.result);
      say(t("form.drop-read.text", { name: file.name, then: then }), false);
    };

    reader.readAsText(file);
  });

  return zone;
}

// listControl is the field whose value is one of a set the server named. A box
// would take anything and leave the refusal to come back from the server, while
// a list cannot hold a value that is not on it, so there is nothing to check
// and nothing to report under it.
//
// An option is either the value itself, which is what a list of settings the
// server named looks like, or a pair: the value the server knows it by and the
// word the screen says it in. The pair is what a value this UI made up needs,
// since that one is a word to be translated and the value behind it is not.
//
// The options are built as elements with their text set as text, the rule every
// value drawn here follows.
function listControl(field) {
  const select = document.createElement("select");

  for (const option of field.options) {
    const said = option !== null && typeof option === "object"
      ? option
      : { value: option, text: option };
    const node = element("option", said.text);

    node.value = said.value;
    select.appendChild(node);
  }

  if (field.value !== undefined && field.value !== null) {
    select.value = String(field.value);
  }

  return select;
}

// showFormRow puts a row of a form on the screen or takes it away.
//
// The hidden property alone does not do it here. What the browser attaches to
// [hidden] is a display of none, and a row of a form is given a display by the
// stylesheet of this page, which wins over it: the row would carry the
// property and go on being drawn. The display is therefore set on the row
// itself, which beats both, and the property is set as well so that a row
// which is not on the screen is also not announced as being there.
function showFormRow(row, shown) {
  row.hidden = !shown;
  row.style.display = shown ? "" : "none";
}

// buildForm draws a form and hands the values to onSubmit. The values are read
// out of the inputs at submit time rather than tracked on every keystroke, so
// there is one place that knows what the form holds.
function buildForm(spec) {
  const form = document.createElement("form");

  // variant, where a caller passes one, marks a form that is not like the ones
  // around it. It is the same word the card it would otherwise be drawn as
  // carries, so a form and a section that say the same thing look the same.
  form.className = spec.variant === undefined ? "card" : "card " + spec.variant;
  form.dataset.form = spec.name;
  form.appendChild(element("h2", spec.legend));

  // What a form has to say before the first box goes here. A paragraph put
  // above the form instead would be a separate card, and the sentence that
  // says what a press cannot be taken back from belongs inside the box that
  // holds the button.
  if (spec.intro !== undefined) {
    for (const node of spec.intro) {
      form.appendChild(node);
    }
  }

  const inputs = {};
  const problems = {};
  const rows = {};

  for (const field of spec.fields) {
    const row = document.createElement("div");
    row.className = "field";

    rows[field.name] = row;

    const id = spec.name + "-" + field.name;
    const label = element("label", field.label);
    label.htmlFor = id;

    const input = field.options === undefined ? textControl(field) : listControl(field);
    input.id = id;
    input.name = field.name;
    input.dataset.field = field.name;

    inputs[field.name] = input;

    row.appendChild(label);
    row.appendChild(input);

    // A field that takes a file offers somewhere to drop one. It goes under
    // the box, so what is read lands in the box the operator is looking at.
    if (field.drop !== undefined) {
      row.appendChild(dropArea(input, field.drop));
    }

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

    // A field may warn about a value without refusing it. check is the other
    // one, and it stops the form: nothing is sent while it has something to
    // say. This one is for a value that is allowed and worth a second look,
    // where refusing would be deciding for the operator something only they
    // can know about the machine on the other end.
    //
    // It is drawn again on every keystroke, so what it says is about the value
    // in the box rather than about the one the form was opened with.
    if (field.advise !== undefined) {
      const advice = element("small", "");

      advice.className = "advice";
      advice.dataset.advice = field.name;

      const sayAdvice = function () {
        // The value is read the way the submit reads it. A checkbox carries
        // "on" in value whether it is ticked or not, so advice given that
        // would say the same thing in both states.
        const said = field.advise(input.type === "checkbox" ? input.checked : input.value);

        advice.textContent = said;
        advice.hidden = said === "";
      };

      input.addEventListener("input", sayAdvice);
      sayAdvice();

      row.appendChild(advice);
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

  // A field that is only asked for while another one is set to a particular
  // value. It is wired once every field exists, since the one that decides may
  // be drawn after the one it decides about.
  //
  // The row is hidden and shown rather than drawn again, so what was typed
  // into it is still there after a look at one of the other values. What is
  // compared is the value of the deciding control, which is what a list
  // carries; a checkbox carries "on" whether it is ticked or not, so it is not
  // one of these.
  for (const field of spec.fields) {
    if (field.shownWhen === undefined) {
      continue;
    }

    const deciding = inputs[field.shownWhen.field];
    const row = rows[field.name];

    const showIt = function () {
      showFormRow(row, deciding.value === field.shownWhen.is);
    };

    deciding.addEventListener("input", showIt);
    showIt();
  }

  const buttons = document.createElement("div");
  buttons.className = "buttons";

  const submit = document.createElement("button");
  submit.type = "submit";
  submit.textContent = spec.submitLabel;
  submit.dataset.action = spec.name + "-submit";

  // A submit that takes something away rather than storing it says so, the way
  // the delete button in a row does.
  if (spec.submitVariant !== undefined) {
    submit.className = spec.submitVariant;
  }

  buttons.appendChild(submit);

  if (spec.onCancel !== undefined) {
    buttons.appendChild(actionButton(t("common.cancel.button"), spec.name + "-cancel", spec.onCancel));
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
      // The check is handed the whole form as well as its own value, because
      // one of them is about a pair: a new password and the box it is typed
      // into a second time are only wrong together.
      //
      // A box that is not on the screen is not checked. What it holds is not
      // what the form is asking for while it is away, and a refusal drawn
      // under a row nobody can see would stop the form with nothing on the
      // screen saying why.
      const message = field.check === undefined || rows[field.name].hidden
        ? ""
        : field.check(values[field.name], values);
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

// openModal puts a panel over the screen and hands back what closed it.
//
// The panel is built from what the caller hands in: a title, the nodes that go
// in the body, and the buttons along the bottom. Every button carries a value,
// and that value is what the promise settles with, so the caller learns which
// press closed the panel rather than only that it closed. A panel that was
// dismissed instead of pressed settles with null: Esc, the backdrop and the
// close in the corner all mean the same thing, which is that nothing was
// chosen.
//
// A button may carry a press of its own instead of a value, and then it is that
// press which decides whether the panel goes. It is what a panel that is worked
// in rather than answered needs, because the answer to what it sends can be a
// refusal that the operator has to see with their work still in front of them.
//
// It is appended to the body and never to #app. #app is emptied by every draw,
// the periodic refresh of the status screen included, so a panel put inside it
// would be taken down by a tick of a screen the operator is not even looking
// at any more.
function openModal(spec) {
  return new Promise(function (resolve) {
    // What had the keyboard before the panel went up. It is where the focus
    // goes back to when the panel leaves, so the operator carries on from the
    // button they pressed instead of from the top of the page.
    const opener = document.activeElement;

    const backdrop = document.createElement("div");
    backdrop.className = "modal-backdrop";
    backdrop.dataset.modal = spec.name;

    const panel = document.createElement("div");
    panel.className = "modal-panel";
    panel.dataset.modalPanel = spec.name;

    // The panel is told to screen readers as a dialog, and is given the title
    // as its name so that what it is for is read out when the focus arrives.
    // It takes focus itself as well, which is what happens when there is
    // nothing inside it that can.
    const titleId = "modal-" + spec.name + "-title";
    const heading = element("h2", spec.title);
    heading.id = titleId;

    panel.setAttribute("role", "dialog");
    panel.setAttribute("aria-modal", "true");
    panel.setAttribute("aria-labelledby", titleId);
    panel.tabIndex = -1;
    panel.appendChild(heading);

    // The body is a scroller of its own. What is long scrolls inside the panel
    // and not behind it, which is the whole reason the page is held still.
    const body = document.createElement("div");
    body.className = "modal-body";

    for (const node of spec.body === undefined ? [] : spec.body) {
      body.appendChild(typeof node === "string" ? element("p", node) : node);
    }

    panel.appendChild(body);

    const buttons = document.createElement("div");
    buttons.className = "buttons modal-buttons";

    for (const button of spec.buttons === undefined ? [] : spec.buttons) {
      // A button that carries a press of its own decides whether the panel
      // goes. It is handed the node it was pressed on, so that it can hold the
      // button down while it runs, and the close, so that a press that is
      // finished with the panel takes it away. That is what a panel the
      // operator works in needs: a save the server refused has to leave what
      // they entered where it is, along with the panel it is in.
      const node = actionButton(button.label, spec.name + "-" + button.name, function () {
        if (button.press === undefined) {
          close(button.value === undefined ? button.name : button.value);

          return;
        }

        return button.press(node, close);
      }, button.variant);

      buttons.appendChild(node);
    }

    panel.appendChild(buttons);
    backdrop.appendChild(panel);

    const record = { panel: panel, close: close };

    // closed says the panel is already on its way out. A second press, or a
    // press and an Esc in the same moment, would otherwise settle the promise
    // twice and release the page twice: the second release would let go of a
    // lock that the panel underneath still needs.
    let closed = false;

    function close(value) {
      if (closed) {
        return;
      }

      closed = true;

      document.removeEventListener("keydown", onKeyDown, true);

      const at = modalStack.indexOf(record);
      if (at !== -1) {
        modalStack.splice(at, 1);
      }

      if (backdrop.parentNode !== null) {
        backdrop.parentNode.removeChild(backdrop);
      }

      // The page is let go only when nothing is left over it. Every way out of
      // the panel comes through here, so there is one place the lock is
      // released and no way of closing that skips it.
      if (modalStack.length === 0) {
        document.body.classList.remove("modal-open");
        window.scrollTo(lockedAtX, lockedAtY);
      }

      // The button that opened the panel may have been drawn again while it
      // was up, and the node that was noted is then no longer on the page.
      // Focus is only put back where there is something to put it on.
      //
      // It is put back without scrolling to it. A browser handed the focus
      // scrolls what it lands on into view, which here would be scrolling away
      // from the position just put back: the page could not move while the
      // panel was up, so what the focus is going back to is where it was left.
      if (opener !== null && typeof opener.focus === "function" && document.body.contains(opener)) {
        opener.focus({ preventScroll: true });
      }

      resolve(value === undefined ? null : value);
    }

    // A press that began inside the panel and ended on the backdrop is a drag,
    // which is what selecting the text of a message looks like when the hand
    // runs past the edge. Only a press that both began and ended on the
    // backdrop closes.
    let pressedBackdrop = false;

    backdrop.addEventListener("mousedown", function (event) {
      pressedBackdrop = event.target === backdrop;
    });

    backdrop.addEventListener("click", function (event) {
      const onBackdrop = event.target === backdrop && pressedBackdrop;

      pressedBackdrop = false;

      if (onBackdrop) {
        close(null);
      }
    });

    function onKeyDown(event) {
      // Only the panel on top answers. The ones under it are covered by it and
      // are not what the operator is looking at.
      if (modalStack[modalStack.length - 1] !== record) {
        return;
      }

      if (event.key === "Escape") {
        event.preventDefault();

        close(null);

        return;
      }

      if (event.key === "Tab") {
        holdFocus(event, panel);
      }
    }

    // Where the page is being read. It is taken before the lock goes on and
    // put back when it comes off, because a browser that cannot scroll an
    // element has nothing to keep a scroll position on: the position survives
    // the lock in some and is lost in others, and what the reader would see
    // there is the list back at the first row.
    const lockedAtX = window.scrollX;
    const lockedAtY = window.scrollY;

    document.body.appendChild(backdrop);
    document.body.classList.add("modal-open");
    modalStack.push(record);
    document.addEventListener("keydown", onKeyDown, true);

    // The keyboard is moved into the panel, so that the next key press is
    // answered by the panel and not by the screen behind it.
    const reachable = modalFocusables(panel);

    (reachable.length === 0 ? panel : reachable[0]).focus();
  });
}

// modalFocusables is what the keyboard can reach inside a panel, in the order
// Tab would reach it.
//
// The list is read at the moment it is needed rather than kept from when the
// panel was built. The body belongs to the caller and a form in it can grow a
// box or lose one while the panel is up, and a list made once would send Tab
// to something that is no longer there.
function modalFocusables(panel) {
  const nodes = panel.querySelectorAll(
    "a[href], button, input, select, textarea, [tabindex]"
  );

  return Array.prototype.filter.call(nodes, function (node) {
    return !node.disabled && !node.hidden && node.tabIndex !== -1;
  });
}

// holdFocus keeps Tab inside the panel. Tab past the last thing in it goes
// back to the first, and Shift+Tab before the first goes to the last.
//
// Without this the keyboard walks out of the panel and onto the screen behind
// it, which is a screen the operator cannot see past the backdrop and cannot
// press with the mouse. What they would have is a caret they have lost.
function holdFocus(event, panel) {
  const reachable = modalFocusables(panel);

  if (reachable.length === 0) {
    event.preventDefault();

    panel.focus();

    return;
  }

  const first = reachable[0];
  const last = reachable[reachable.length - 1];
  const at = document.activeElement;
  const inside = at !== null && panel.contains(at) && at !== panel;

  if (event.shiftKey) {
    if (!inside || at === first) {
      event.preventDefault();

      last.focus();
    }

    return;
  }

  if (!inside || at === last) {
    event.preventDefault();

    first.focus();
  }
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

// checkPasswordConfirmation says what is wrong with the second box a new
// password is typed into, or "" when nothing is.
//
// The two are compared here and nowhere else. The server is never sent the
// second copy: handed the same string twice it has nothing to learn from the
// second one, and what the box is there for is a typo, which is made here.
function checkPasswordConfirmation(value, password) {
  return String(value) === String(password)
    ? ""
    : t("form.password-mismatch.error");
}

// checkPort says what is wrong with a port, or "" when nothing is. The box only
// takes digits, so what is left to catch is an empty one and a number outside
// what the server accepts.
function checkPort(value) {
  const trimmed = String(value).trim();

  if (trimmed === "") {
    return t("form.port-empty.error");
  }

  const port = Number(trimmed);
  if (!Number.isInteger(port) || port < minPort || port > maxPort) {
    return t("form.port-range.error", { least: minPort, most: maxPort });
  }

  return "";
}

// checkSeconds says what is wrong with a period, or "" when nothing is. The
// server refuses a period of zero, and a loop that is asked to run every zero
// seconds has no period at all.
function checkSeconds(value) {
  const trimmed = String(value).trim();

  if (trimmed === "") {
    return t("form.seconds-empty.error");
  }

  const seconds = Number(trimmed);
  if (!Number.isInteger(seconds) || seconds < 1) {
    return t("form.seconds-min.error");
  }

  return "";
}

// checkCount says what is wrong with one of the numbers the log rotation is
// held to, or "" when nothing is. Zero is a value the server takes: it is how
// the rotation is told to keep no bound at all.
function checkCount(value) {
  const trimmed = String(value).trim();

  if (trimmed === "") {
    return t("form.count-empty.error");
  }

  const count = Number(trimmed);
  if (!Number.isInteger(count) || count < 0) {
    return t("form.count-negative.error");
  }

  return "";
}

// checkPath says what is wrong with a file path, or "" when nothing is. What
// makes a path usable is decided by the filesystem the server runs on, so the
// one thing checked here is the one thing that is wrong everywhere: an empty
// box. A key file that is empty stops the next startup, and there is no
// configuration file left to put it back in.
function checkPath(value) {
  if (String(value).trim() === "") {
    return t("form.path-empty.error");
  }

  return "";
}

// checkIP says what is wrong with an address, or "" when nothing is.
function checkIP(value) {
  const trimmed = String(value).trim();

  if (trimmed === "") {
    return t("form.ip-empty.error");
  }

  if (!isIPv4(trimmed) && !isIPv6(trimmed)) {
    return t("form.ip-shape.error");
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
  return t("form.password-bytes.hint", {
    bytes: passwordBytes(value),
    least: minPasswordBytes,
    most: maxPasswordBytes
  });
}

// passwordBytes is how long a password is in the unit the server counts it in.
function passwordBytes(value) {
  return new TextEncoder().encode(String(value)).length;
}

// checkPasswordLength says what is wrong with the length of a password, or ""
// when nothing is. It is the rule the server holds, checked here so that a
// password that is a byte short is reported under the box rather than after the
// password has been sent over the wire to be refused.
function checkPasswordLength(value) {
  const bytes = passwordBytes(value);

  if (bytes < minPasswordBytes) {
    return t("form.password-short.error", { least: minPasswordBytes, bytes: bytes });
  }

  if (bytes > maxPasswordBytes) {
    return t("form.password-long.error", { most: maxPasswordBytes, bytes: bytes });
  }

  return "";
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
    return t("common.never.text");
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
//
// It is told to read left to right on its own, because on a page that reads
// the other way the date and the clock are two runs of digits with a space
// between them, and the space takes the direction of the page: the clock is
// then drawn first and the date after it. The word for never is left to the
// page, since it is a word in the language of the page.
function timeCell(value) {
  const text = formatTime(value);
  const node = element("span", text);

  node.className = "stamp";

  if (text !== t("common.never.text")) {
    node.dir = "ltr";
  }

  return node;
}

// pad keeps the columns of a timestamp the same width.
function pad(value) {
  return String(value).padStart(2, "0");
}

// formatBytes writes a size the way it is read rather than as a bare count of
// bytes. The log screen says with it how much of the file was touched to fill
// the table next to how large the file is, and those two numbers are only worth
// putting side by side if both can be taken in at a glance.
function formatBytes(value) {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    return "?";
  }

  const units = [t("common.unit-bytes.text"), t("common.unit-kb.text"),
    t("common.unit-mb.text"), t("common.unit-gb.text")];

  let size = value;
  let unit = 0;

  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit += 1;
  }

  // Bytes are whole things and are written as they are. The larger units are
  // divided down and keep one decimal, so that 1.4 MB is not shown as 1 MB.
  return (unit === 0 ? String(size) : size.toFixed(1)) + " " + units[unit];
}

// plural picks the key a count is read with, so that one tunnel is not
// reported as "1 tunnels". The two keys handed in are the English pair, and
// they differ in the one part that says which of the two they are.
//
// Which part a count falls in is the language's to say and not English's:
// French reads zero the way it reads one, Russian reads two and five apart, and
// Arabic has six parts. The browser is asked which part the count is in, and
// the catalog is asked for a key with that part written where the pair writes
// one and many. A language that has written no such key is read with its many
// key, which is what its translators wrote for every count but one, and the
// one key is the last resort. A -few key added to a catalog later is picked up
// from then on without anything here changing.
//
// The key is looked for in the language first and in English after, so that a
// language which has both of the pair is read with its own many key rather
// than with a part English has written and it has not.
//
// A browser without Intl.PluralRules reads every count the English way.
function plural(count, one, many) {
  const part = pluralPart(count);

  if (part === null) {
    return count === 1 ? one : many;
  }

  // The keys are the same up to the part and the same after it, so the head is
  // where they stop agreeing. A pair that is not shaped that way is read the
  // English way, since there is no place in it to write another part.
  let head = 0;

  while (head < one.length && head < many.length && one[head] === many[head]) {
    head += 1;
  }

  if (one.slice(head, head + 3) !== "one" || many.slice(head, head + 4) !== "many" ||
      one.slice(head + 3) !== many.slice(head + 4)) {
    return part === "one" ? one : many;
  }

  const wanted = [one.slice(0, head) + part + one.slice(head + 3), many, one];

  for (const table of [texts, baseTexts]) {
    for (const key of wanted) {
      if (entry(table, key) !== null) {
        return key;
      }
    }
  }

  return many;
}

// pluralPart is which of the parts of the language a count is in: zero, one,
// two, few, many or other, as the browser works it out for the language the
// page is in. It is null where the browser cannot say.
function pluralPart(count) {
  if (typeof Intl === "undefined" || typeof Intl.PluralRules !== "function") {
    return null;
  }

  try {
    return new Intl.PluralRules(currentLang()).select(count);
  } catch (error) {
    return null;
  }
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

      loadedVersion = payload.version;

      paintVersion();
    })
    .catch(function () {});
}

// paintVersion writes the number in the corner. It is apart from the fetch
// because the two do not arrive together and neither waits on the other: the
// number comes from the server, the words around it come from a catalog, and
// whichever is second is what puts the line up. A change of language calls this
// again, which is why the number is kept rather than fetched once and forgotten.
function paintVersion() {
  if (loadedVersion === null) {
    return;
  }

  // The version is written the way the tag and the release name it, in every
  // language: it is a name to match against a release, not a sentence.
  document.getElementById("versionTag").textContent = "v" + loadedVersion;
}

// storedTheme is what was picked on this browser, or null where nothing was.
// The read is inside the try and not only the write: a browser that has storage
// turned off throws on the access itself, and a page that let that through
// would stop before it drew anything.
function storedTheme() {
  let picked = null;

  try {
    picked = window.localStorage.getItem(themeKey);
  } catch (error) {
    return null;
  }

  return picked === "light" || picked === "dark" ? picked : null;
}

// rememberTheme keeps the pick for the next visit. A browser that will not keep
// it is not an error to report: the screen is already in the theme that was
// asked for, and what is lost is only that the next page starts from what the
// browser prefers rather than from what was pressed here.
function rememberTheme(name) {
  try {
    window.localStorage.setItem(themeKey, name);
  } catch (error) {
    return;
  }
}

// defaultTheme is what a browser that has not been told otherwise gets. It is
// the dark one, and it is not read off what the browser prefers: this page is a
// console left open beside other work and is drawn for the dark side, where
// what a browser prefers is a guess made about every page at once. The same
// name is written into index.html, which settles the theme before the first
// paint; the two have to say the same thing.
function defaultTheme() {
  return "dark";
}

// currentTheme is what the page is in now. It is read off <html>, which is the
// one place the theme is held: the stylesheet paints from that attribute.
function currentTheme() {
  return document.documentElement.getAttribute("data-theme") === "dark" ? "dark" : "light";
}

// applyTheme paints the page in a theme and tells the switch what the next
// press would do.
function applyTheme(name) {
  document.documentElement.setAttribute("data-theme", name);

  labelThemeToggle();
}

// labelThemeToggle says what the switch is and what a press would do, for a
// reader that is not looking at it. The label names where a press goes rather
// than where the page is, because that is what the operator is deciding; what
// the page is in now is aria-pressed, which is a different question and is
// answered separately.
//
// It is apart from applyTheme because the two happen at different moments. The
// colour is settled before anything is painted, and the words wait on a catalog
// that is still being fetched; until it arrives the switch stays as index.html
// ships it, unlabelled and hidden, rather than carrying an English word on a
// page that was asked for in another language. The language calls this again
// once it has what it needs.
function labelThemeToggle() {
  const button = document.getElementById("themeToggle");
  if (button === null) {
    return;
  }

  const dark = currentTheme() === "dark";

  // The state goes on whatever the theme is, catalog or no catalog. It is an
  // attribute and not a word, so it is right before the words have arrived,
  // and it is what a reader that cannot see the knob move is told.
  button.setAttribute("aria-pressed", dark ? "true" : "false");

  if (!textsLoaded()) {
    return;
  }

  // Nothing is written into the button: it holds a drawing, and text put on it
  // would replace the track and the knob. What the switch says in words it says
  // to a reader, through the label below.
  const next = dark ? t("theme.light.label") : t("theme.dark.label");

  button.setAttribute("aria-label", t("theme.switch.aria", { theme: next }));
  button.hidden = false;
}

// themeSwapClass is on <html> only while a change of theme is being drawn. The
// stylesheet hangs the transitions on it, so the colours move for a change and
// stay instant for everything else; see the rule it is named in.
const themeSwapClass = "theme-swapping";

// The timer that takes the class off. It is held so a second press does not
// leave the first press's timer to end the second one's turn: what the class is
// on for is the change that is happening now.
let themeSwapEnding = null;

// swapTheme changes the theme with the colours moving.
//
// It is apart from applyTheme because the first paint uses that one. There the
// theme is settled before anything is on the screen, so there is nothing to
// move from, and a page that faded in from the other theme on every load would
// be the cost of writing it as one function.
function swapTheme(name) {
  if (name === currentTheme()) {
    return;
  }

  const root = document.documentElement;

  root.classList.add(themeSwapClass);

  if (themeSwapEnding !== null) {
    window.clearTimeout(themeSwapEnding);
  }

  applyTheme(name);

  // The wait is read off the stylesheet rather than written twice. Somebody who
  // asked for less movement has it at zero there, and this then takes the class
  // off on the next turn of the loop instead of holding it for a third of a
  // second over a change that already happened.
  // A little longer than the change is given, so that nothing is still moving
  // when the class goes: a transition cut short jumps the rest of the way, and
  // the jump is what would read as the change being late. The margin costs
  // nothing, because what the class does while nothing is moving is nothing.
  themeSwapEnding = window.setTimeout(function () {
    root.classList.remove(themeSwapClass);
    themeSwapEnding = null;
  }, themeSwapMs() * 2);
}

// themeSwapMs is how long the stylesheet says a change of theme takes, in
// milliseconds. A value it cannot read is treated as no wait at all, which
// leaves the change instant rather than leaving the class on.
function themeSwapMs() {
  const said = window.getComputedStyle(document.documentElement)
    .getPropertyValue("--theme-swap").trim();

  if (said.endsWith("ms")) {
    return Number(said.slice(0, -2)) || 0;
  }

  if (said.endsWith("s")) {
    return (Number(said.slice(0, -1)) || 0) * 1000;
  }

  return 0;
}

// setUpTheme puts the switch to work. The theme is worked out again here rather
// than taken off <html>, so the page is right even where the head script did
// not run, and the browser is followed for as long as nothing has been picked:
// an operator who never pressed the switch gets the change they made to their
// system without having to load the page again.
function setUpTheme() {
  const picked = storedTheme();

  applyTheme(picked === null ? defaultTheme() : picked);

  const button = document.getElementById("themeToggle");
  if (button !== null) {
    button.addEventListener("click", function () {
      const next = currentTheme() === "dark" ? "light" : "dark";

      swapTheme(next);
      rememberTheme(next);
    });
  }

}

// t is the word a key stands for, in the language the page is in. It is one
// letter because it is what every string on every screen is fetched through,
// and a longer name would be read past rather than read.
//
// A key the chosen language has no entry for falls through to English, so a
// screen that has been translated half way is half translated and never half
// blank. A key that is in neither is handed back as it was written, which is
// wrong on the screen on purpose: it is the one failure that has to be visible,
// and a test over the catalogs keeps it from reaching a release.
//
// What comes back is a string and is put on the page with textContent, which is
// the rule everything here follows. Nothing in a catalog is ever markup, so a
// translation cannot bring an element with it.
function t(key, values) {
  let template = entry(texts, key);

  if (template === null) {
    template = entry(baseTexts, key);
  }

  if (template === null) {
    return key;
  }

  if (values === undefined) {
    return template;
  }

  // The values are written in one pass over the sentence, so what is written in
  // is never read again. A Host named "{name}" is a name and not a second place
  // to fill, which is what a fill that went value by value would make of it.
  return template.replace(placeholder, function (whole, name) {
    if (!Object.prototype.hasOwnProperty.call(values, name)) {
      return whole;
    }

    return String(values[name]);
  });
}

// entry is one string out of one catalog, or null where the catalog has not
// arrived, has no such key, or holds something other than a string under it.
// The last of those is what a catalog edited into the wrong shape looks like,
// and it falls back like a missing key rather than putting an object on screen.
function entry(table, key) {
  if (table === null || !Object.prototype.hasOwnProperty.call(table, key)) {
    return null;
  }

  return typeof table[key] === "string" ? table[key] : null;
}

// textsLoaded says whether there is anything to draw words from yet. Everything
// that writes a word outside #app asks first, because those elements are drawn
// once and are not redrawn by a screen.
function textsLoaded() {
  return baseTexts !== null;
}

// languageFor is the entry for a code, or null where there is no such language.
function languageFor(code) {
  for (const language of languages) {
    if (language.code === code) {
      return language;
    }
  }

  return null;
}

// storedLang is what was picked on this browser, or null where nothing was or
// where what is kept is no longer a language this UI has. The read is inside
// the try for the reason the theme read is: a browser with storage turned off
// throws on the access itself.
function storedLang() {
  let picked = null;

  try {
    picked = window.localStorage.getItem(langKey);
  } catch (error) {
    return null;
  }

  return languageFor(picked) === null ? null : picked;
}

// rememberLang keeps the pick for the next visit. A browser that will not keep
// it is not an error to report: the page is already in the language that was
// asked for, and what is lost is only that the next one starts from what the
// browser asks for rather than from what was picked here.
function rememberLang(code) {
  try {
    window.localStorage.setItem(langKey, code);
  } catch (error) {
    return;
  }
}

// browserLang is the first language the browser asks for that there is a
// catalog for. index.html works the same thing out in the head, and this is
// what is used where that script did not run.
//
// A tag is matched whole before it is matched by its language alone, so a
// browser set to pt-BR is answered with the Brazilian catalog and one set to
// pt-PT is answered with Portuguese rather than dropped to English.
function browserLang() {
  let wanted = window.navigator.languages;

  if (wanted === undefined || wanted === null || wanted.length === 0) {
    wanted = window.navigator.language === undefined ? [] : [window.navigator.language];
  }

  for (const tag of wanted) {
    const lower = String(tag).toLowerCase();

    for (const language of languages) {
      if (language.code.toLowerCase() === lower) {
        return language.code;
      }
    }

    for (const language of languages) {
      if (language.code.toLowerCase().split("-")[0] === lower.split("-")[0]) {
        return language.code;
      }
    }
  }

  return baseLang;
}

// currentLang is the language to draw in: what was picked here, then what the
// installation was set up to show, then what the browser asks for, then
// English.
//
// The pick of the browser wins over the setting on purpose. The setting is what
// an installation is drawn in for somebody who has said nothing about it, and
// somebody who has said something has said it on the screen they are reading.
//
// The setting is only ever in hand behind the login, since reading it needs a
// session, and it is null until then: the login screen is settled by the pick
// and the browser, which is the whole of this rule for a client that has not
// signed in.
//
// The head script of index.html has been down the same road without the
// setting, and where the setting says nothing its answer is the one used rather
// than worked out again: it is the one the first paint was laid out with, and a
// second run that disagreed with it would turn the page round after it had been
// drawn.
function currentLang() {
  const picked = storedLang();

  if (picked !== null) {
    return picked;
  }

  if (installationLang !== null && languageFor(installationLang) !== null) {
    return installationLang;
  }

  const boot = window.tmBoot;

  if (boot !== undefined && boot !== null && languageFor(boot.lang) !== null) {
    return boot.lang;
  }

  return browserLang();
}

// loadInstallationLang reads the language this installation draws a browser
// that has picked none in, and leaves it where currentLang reads it.
//
// It is asked for with a fetch of its own rather than through apiCall, because
// what it meets on the login screen is the answer that says there is no
// session, and apiCall turns that into a move to the login: the screen this
// would be running on. There is nothing to report either way. A setting that
// cannot be read leaves the page in the language the browser asked for, which
// is where it would have been without the setting at all.
async function loadInstallationLang() {
  if (storedLang() !== null) {
    // The pick of this browser wins over the setting, so there is nothing the
    // answer could change and no call to make.
    return;
  }

  let response;

  try {
    response = await fetch("/api/settings", {
      headers: { Accept: "application/json" },
      credentials: "same-origin"
    });
  } catch (error) {
    return;
  }

  if (!response.ok) {
    return;
  }

  let payload;

  try {
    payload = await response.json();
  } catch (error) {
    return;
  }

  if (payload === null || payload.data === null || payload.data === undefined) {
    return;
  }

  const code = payload.data.ui_default_language;

  if (typeof code === "string") {
    installationLang = code;
  }
}

// followInstallationLang reads the setting again and draws the page in it if
// that changes what the language is.
//
// It is what the two moments that can change the answer call: a login, which is
// where the setting becomes readable at all, and a save of the settings, which
// is where it becomes something else. Neither is a moment the page is reloaded
// at, and without this the operator would go on reading a screen in the
// language of their browser until they did reload it.
async function followInstallationLang() {
  const was = currentLang();

  await loadInstallationLang();

  const now = currentLang();

  if (now !== was) {
    await applyLang(now);
  }
}

// forgetInstallationLang drops the setting when the session it was read
// through ends, and draws the page in the language that is left: what was
// picked here, then what the browser asks for. The login screen is settled by
// those two alone, and a tab that has just signed out is drawn the way a new
// tab would be rather than staying in the language of the installation.
async function forgetInstallationLang() {
  const was = currentLang();

  installationLang = null;

  const now = currentLang();

  if (now !== was) {
    await applyLang(now);
  }
}

// fetchCatalog asks for one language file. A refusal comes back as null rather
// than as a throw: what a missing catalog costs is words, and the caller has to
// carry on and draw the screens either way.
function fetchCatalog(code) {
  return fetch(langPath + code + ".json", {
    headers: { Accept: "application/json" },
    credentials: "same-origin"
  })
    .then(function (response) {
      return response.ok ? response.json() : null;
    })
    .catch(function () {
      return null;
    });
}

// loadTexts puts the two catalogs a draw needs in place: the one that was asked
// for and the English one it falls back to key by key.
//
// The English one is fetched once and kept. Every later change of language
// needs only the language being changed to, so switching costs one file and not
// two, and the fallback cannot go missing part way through a session.
//
// Where the head script started the same two fetches, those are what is waited
// on rather than two more: they went out while the head was still being parsed
// and have been travelling alongside screens.js and app.js ever since.
async function loadTexts(code) {
  const boot = window.tmBoot !== undefined && window.tmBoot !== null &&
    window.tmBoot.lang === code ? window.tmBoot : null;

  if (baseTexts === null) {
    const loaded = await (boot === null ? fetchCatalog(baseLang) : boot.base);

    baseTexts = loaded === null ? {} : loaded;
  }

  if (code === baseLang) {
    texts = baseTexts;

    return;
  }

  const loaded = await (boot === null ? fetchCatalog(code) : boot.text);

  texts = loaded === null ? baseTexts : loaded;
}

// applyLang draws the page in a language: it fetches what is needed first, so
// that nothing is ever relabelled twice, and then puts the language on <html>
// and the words on everything that lives outside a screen.
//
// lang is what tells a screen reader which voice to read the page in and what a
// browser picks a font by. dir is what turns the layout round, and it is set on
// every language and not only on the one that needs it: left on rtl after a
// change away from Arabic the page would stay reversed.
async function applyLang(code) {
  await loadTexts(code);

  const language = languageFor(code);

  document.documentElement.setAttribute("lang", code);
  document.documentElement.setAttribute("dir", language !== null && language.rtl ? "rtl" : "ltr");

  labelThemeToggle();
  labelLanguagePicker(code);
  paintVersion();
}

// fillLanguagePicker puts the thirteen names in the list. It runs before any
// catalog has arrived, because the names are not translated: each is written in
// the language it names, and that is the whole point of it.
function fillLanguagePicker() {
  const picker = document.getElementById("langPick");
  if (picker === null) {
    return;
  }

  for (const language of languages) {
    const option = document.createElement("option");

    option.value = language.code;
    option.textContent = language.name;

    // The row is told what language it is in, which is not the language the
    // page is in. It is what a browser picks the font for that one line by, and
    // it is how the Arabic name reads the way it is written while the list
    // around it runs the other way.
    option.lang = language.code;
    option.dir = language.rtl ? "rtl" : "ltr";

    picker.appendChild(option);
  }
}

// labelLanguagePicker points the list at the language that is up and gives it
// the name a screen reader announces it by. It is what unhides the list, so
// what is shown is never a control whose purpose has no words on it yet.
function labelLanguagePicker(code) {
  const picker = document.getElementById("langPick");
  if (picker === null || !textsLoaded()) {
    return;
  }

  picker.value = code;
  picker.setAttribute("aria-label", t("lang.pick.aria"));
  picker.hidden = false;
}

// setUpLanguage puts the list to work and draws the page in the language that
// was settled on. What it hands back is what the first screen waits for: the
// words have to be in hand before anything is drawn, or the first screen would
// go up in English and be replaced a moment later.
function setUpLanguage() {
  fillLanguagePicker();

  const picker = document.getElementById("langPick");
  if (picker !== null) {
    picker.addEventListener("change", function () {
      const code = picker.value;

      // The pick is kept before the catalog is fetched. It is the operator's
      // choice either way, and a fetch that fails should not leave the next
      // visit in a language they have already said they do not want.
      rememberLang(code);

      run(function () {
        return applyLang(code).then(function () {
          // The screen is entered again rather than redrawn, so that what a
          // screen built once on the way in is built again in the new words.
          showScreen(currentScreen);
        });
      });
    });
  }

  // The setting is asked for before the first draw and not after it, so that a
  // page whose language comes from the installation goes up in that language
  // once instead of being drawn in the language of the browser and turned round
  // a moment later. It costs one call, and only where nothing was picked here.
  return loadInstallationLang().then(function () {
    return applyLang(currentLang());
  });
}

// The scroll is watched for one thing: when it last happened. The listener is
// passive, so nothing it does can hold up the scrolling it is watching.
//
// It is caught on the way down and not on the way up. A scroll event does not
// bubble when what scrolled is an element rather than the document, so a
// listener waiting at the window never hears the ones that matter most here:
// the wide table on the status screen is inside a scroller of its own, and
// dragging that sideways to read the end of an error was a movement this could
// not see. Capturing puts the listener on the path the event does take.
document.addEventListener("scroll", function () {
  scrolledAt = Date.now();
}, { capture: true, passive: true });

// A pointer is watched for the same reason the scroll is, and before it: it
// comes down first. Lifting it starts the quiet time rather than ending the
// wait, because what a phone does when a finger leaves is carry on moving.
//
// Both sets of names are listened for, the pointer ones and the touch ones,
// rather than one set being picked. They overlap on most browsers and the two
// say the same thing when they do, so hearing both costs a flag being set to
// what it already is. What it buys is the browser where they do not overlap:
// one that reports a finger only as a touch, or one that cancels the pointer
// the moment a drag turns into a scroll and then says nothing more about it.
// Which browser does which is not something this page can ask.
const heldDownNames = ["pointerdown", "touchstart"];
const letGoNames = ["pointerup", "pointercancel", "touchend", "touchcancel"];

for (const name of heldDownNames) {
  window.addEventListener(name, function () {
    pointerDown = true;
    scrolledAt = Date.now();
  }, { capture: true, passive: true });
}

for (const name of letGoNames) {
  window.addEventListener(name, function () {
    pointerDown = false;
    scrolledAt = Date.now();
  }, { capture: true, passive: true });
}

// A drag and a wheel, which are movement before anything has scrolled.
//
// A scroll event is the result and these are the cause, and the two come apart
// in the cases that matter: a wheel or a trackpad over a list that is already
// at its end moves nothing and fires no scroll, and a finger dragging a page
// fires its moves before the first scroll lands. Both are somebody working the
// page with their hand on it, which is the whole of what is being watched for.
for (const name of ["touchmove", "wheel"]) {
  window.addEventListener(name, function () {
    scrolledAt = Date.now();
  }, { capture: true, passive: true });
}

// What clears a press that was never let go of over this page.
//
// A mouse pressed on a row and released somewhere else - over another window,
// or past the edge of this one - sends its release to whatever it was over, and
// this page hears nothing. Without something to put the flag back, the refresh
// of the status screen would be stopped for as long as the page stayed open.
//
// The move is the cheaper of the two to trust: it says which buttons are held
// at the moment it fires, so the first move back over the page corrects the
// flag whatever happened while the pointer was away. Losing the window clears
// it as well, for the case where the pointer does not come back.
if (window.PointerEvent !== undefined) {
  window.addEventListener("pointermove", function (event) {
    if (pointerDown && event.buttons === 0) {
      pointerDown = false;
      scrolledAt = Date.now();
    }
  }, { passive: true });
}

window.addEventListener("blur", function () {
  pointerDown = false;
});

window.addEventListener("popstate", function () {
  notice = null;
  showScreen(screenName());
});

setUpTheme();
showVersion();

// The first screen waits for the words it is to be drawn with, and for nothing
// else: the colour is already on the page, the version fills itself in when it
// arrives, and the two files being waited on left in the head, before this one
// had been fetched. Drawn without them the screen would go up in English and be
// replaced a moment later, which is the one thing a page that knows what
// language it is in should never do.
//
// A catalog that cannot be fetched at all resolves to an empty table rather
// than hanging, so this always reaches the draw.
run(function () {
  return setUpLanguage().then(function () {
    showScreen(screenName());
  });
});
