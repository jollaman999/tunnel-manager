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
  });
};
