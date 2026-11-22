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
// The key is put only on a request to this same origin, so it is not handed
// to wherever a URL typed into a form points, and not over a key pasted into
// Authorize, which is the one the reader asked for.
//
// responseInterceptor is the other half: a login sent from this page keeps the
// key it is answered with where the screens keep theirs, and a logout drops
// it, the way the screens do. The browser holds one session cookie for this
// origin whichever page signed in, so the key beside it is one as well; a
// login here that left the key of the screens in place would have this page
// and the screens send a key that is not the one of the cookie.
const csrfStoreKey = "tm_csrf";
const csrfHeaderName = "X-CSRF-Token";

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

function withSessionKey(request) {
  let key = null;

  try {
    key = window.localStorage.getItem(csrfStoreKey);
  } catch (error) {
    return request;
  }

  if (key === null || key === "" || sameOrigin(request.url) === null) {
    return request;
  }

  request.headers = request.headers || {};

  if (request.headers[csrfHeaderName] === undefined) {
    request.headers[csrfHeaderName] = key;
  }

  return request;
}

function keepSessionKey(response) {
  const target = sameOrigin(response.url);

  if (target === null || !response.ok) {
    return response;
  }

  try {
    if (target.pathname.endsWith("/api/logout")) {
      window.localStorage.removeItem(csrfStoreKey);
    }

    if (target.pathname.endsWith("/api/login")) {
      const body = response.body;
      const key = body && body.data ? body.data.csrf_token : undefined;

      if (typeof key === "string" && key !== "") {
        window.localStorage.setItem(csrfStoreKey, key);
      }
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
    requestInterceptor: withSessionKey,
    responseInterceptor: keepSessionKey,
  });
};
