// Service-worker registration (UI-05).
//
// A file, not inline, because the CSP is script-src 'self' -- the same reason
// counter.js is a file. It registers the worker at "/sw.js" carrying the asset
// digest as "?v=", read from a meta tag the layout renders. That query is how a
// deploy makes the browser see a new worker (see sw.js), so it must be present.
//
// Registration is entirely optional to the app: a browser without service
// workers, or a registration that fails, loses only the installed-shell nicety.
// Every data path still works over the network, so a failure here is logged and
// swallowed, never surfaced.
"use strict";

if ("serviceWorker" in navigator) {
  window.addEventListener("load", function () {
    var meta = document.querySelector('meta[name="bittabby-asset-version"]');
    var v = meta ? meta.getAttribute("content") : "";
    var url = "/sw.js" + (v ? "?v=" + encodeURIComponent(v) : "");
    navigator.serviceWorker.register(url).catch(function (err) {
      if (window.console && console.warn) {
        console.warn("BitTabby: service worker registration failed", err);
      }
    });
  });
}
