// What the Swagger UI on this page is built with.

// The description is asked for by a path relative to this page, so the page
// works unchanged behind a prefix a reverse proxy puts in front of it. An
// absolute /ui/openapi.json would be the one thing on this page that stops
// being true the moment someone mounts the server somewhere other than the
// root.
//
// BaseLayout is used rather than StandaloneLayout. StandaloneLayout draws a top
// bar holding a box for typing the URL of a description to read, and this
// server serves exactly one; what the bar produced was a picker with a single
// entry in it. It is also the one part that needs a second script,
// swagger-ui-standalone-preset.js, which is three hundred kilobytes this
// binary would then carry for that bar.
//
// deepLinking puts the operation being read into the address, so a link to one
// endpoint can be sent to someone.
//
// tryItOutEnabled opens the form straight away. Every request the page makes
// goes to this same server and is checked by the same session, so there is
// nothing behind the extra click.
//
// validatorUrl is turned off. Left alone, the Swagger UI puts a badge at the
// foot of the page saying whether the description is valid, and it works that
// out by sending the description to validator.swagger.io. That is the one
// fetch this page would make that is not a file of its own, and it would carry
// the shape of this installation's API to somebody else's server.
//
// requestInterceptor is what lets "Try it out" use the session the operator
// signed in with. The server asks every request the session cookie is sent
// with for the key of the session in a header, reads included, and the cookie
// alone is answered as no session. The screens keep the key in local storage
// under csrfStoreKey, which app.js names as well, and this page is served from
// the same origin, so it reads the key from there.
//
// The device value of the browser goes the same way, under deviceStoreKey and
// in deviceHeaderName. It is what lets a login past a hold on the account that
// somebody else brought about, and the server hands it out in no cookie, so a
// login sent from here would be counted like one from a stranger without it.
//
// Both are put only on a request to this same origin, so neither is handed to
// wherever a URL typed into a form points, and neither is put over a value the
// reader set themselves, such as a key pasted into Authorize.
//
// responseInterceptor is the other half: a login sent from this page keeps the
// key and the device value it is answered with where the screens keep theirs,
// the setup and the account change keep the device value they hand out, and a
// logout drops the key and leaves the device value, the way the screens do.
// The browser holds one session cookie for this origin whichever page signed
// in, so the key beside it is one as well; a login here that left the key of
// the screens in place would have this page and the screens send a key that
// is not the one of the cookie.
const csrfStoreKey = "tm_csrf";
const csrfHeaderName = "X-CSRF-Token";
const deviceStoreKey = "tm_device";
const deviceHeaderName = "X-TM-Device";

// handsOutDevice are the paths whose answer carries a new device value.
const handsOutDevice = ["/api/login", "/api/setup", "/api/account"];

// sameOrigin returns the URL of url when it is on this origin, or null.
function sameOrigin(url) {
  let target;

  try {
    target = new URL(url, window.location.href);
  } catch (error) {
    return null;
  }

  return target.origin === window.location.origin ? target : null;
}

// keptValue is what the screens keep under key, or "" where they keep nothing
// or the browser keeps nothing in local storage.
function keptValue(key) {
  try {
    const kept = window.localStorage.getItem(key);

    return kept === null ? "" : kept;
  } catch (error) {
    return "";
  }
}

function withKeptValues(request) {
  if (sameOrigin(request.url) === null) {
    return request;
  }

  for (const [storeKey, headerName] of [
    [csrfStoreKey, csrfHeaderName],
    [deviceStoreKey, deviceHeaderName]
  ]) {
    const value = keptValue(storeKey);

    if (value === "") {
      continue;
    }

    request.headers = request.headers || {};

    if (request.headers[headerName] === undefined) {
      request.headers[headerName] = value;
    }
  }

  return request;
}

function keepHandedOutValues(response) {
  const target = sameOrigin(response.url);

  if (target === null || !response.ok) {
    return response;
  }

  const body = response.body;
  const data = body && body.data ? body.data : {};
  const key = data.csrf_token;
  const device = data.device;

  try {
    if (target.pathname.endsWith("/api/logout")) {
      window.localStorage.removeItem(csrfStoreKey);
    }

    if (target.pathname.endsWith("/api/login") &&
        typeof key === "string" && key !== "") {
      window.localStorage.setItem(csrfStoreKey, key);
    }

    const handed = handsOutDevice.some(function (path) {
      return target.pathname.endsWith(path);
    });

    if (handed && typeof device === "string" && device !== "") {
      window.localStorage.setItem(deviceStoreKey, device);
    }
  } catch (error) {
    return response;
  }

  return response;
}

window.onload = function () {
  window.ui = SwaggerUIBundle({
    url: "../openapi.json",
    dom_id: "#swagger-ui",
    layout: "BaseLayout",
    presets: [SwaggerUIBundle.presets.apis],
    deepLinking: true,
    tryItOutEnabled: true,
    docExpansion: "list",
    validatorUrl: null,
    defaultModelsExpandDepth: 1,
    requestInterceptor: withKeptValues,
    responseInterceptor: keepHandedOutValues,
  });
};
