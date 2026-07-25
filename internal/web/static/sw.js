// BitTabby service worker (UI-05).
//
// A SHELL-ONLY worker, on purpose. It caches the static frame -- stylesheet,
// htmx, the small scripts, the logo, the offline page -- so an installed app
// paints instantly and looks native. It caches NO data. Every navigation goes
// to the network first, because the balance is derived server-side and must
// never be shown stale (LEDGER-03). If you find yourself caching anything on a
// tab route, stop: a cached balance is a wrong balance.
//
// Versioning. The registration URL carries "?v=<asset-digest>" (see
// sw-register.js), so this script reads its own version from that query and
// scopes the cache name to it. A deploy changes the digest, which changes the
// script URL, which makes the browser install a fresh worker that builds a new
// cache and drops the old one in activate. The digest is the same one every
// static URL already carries (web.AssetVersion), so the shell and the cache
// invalidate together. Do not hand-maintain a version string here.
"use strict";

var VERSION = new URL(self.location.href).searchParams.get("v") || "dev";
var CACHE = "bittabby-shell-" + VERSION;

// The static shell. Listed without the "?v=" query and matched with
// ignoreSearch below, so a versioned page request and the offline page's own
// bare reference both hit the same cached copy. The cache name carries the
// version instead, so correctness comes from the cache being rebuilt per
// deploy, not from the URL query.
var SHELL = [
  "/static/app.css",
  "/static/htmx.min.js",
  "/static/counter.js",
  "/static/sw-register.js",
  "/static/logo.svg",
  "/static/offline.html",
  "/static/icon-192.png",
  "/static/manifest.webmanifest"
];

self.addEventListener("install", function (event) {
  event.waitUntil(
    caches.open(CACHE).then(function (cache) {
      return cache.addAll(SHELL);
    }).then(function () {
      // Take over as soon as the new shell is cached, so a deploy's assets are
      // live on the next navigation rather than after every tab is closed.
      return self.skipWaiting();
    })
  );
});

self.addEventListener("activate", function (event) {
  event.waitUntil(
    caches.keys().then(function (keys) {
      return Promise.all(keys.filter(function (k) {
        return k !== CACHE && k.indexOf("bittabby-shell-") === 0;
      }).map(function (k) {
        return caches.delete(k);
      }));
    }).then(function () {
      return self.clients.claim();
    })
  );
});

self.addEventListener("fetch", function (event) {
  var req = event.request;
  if (req.method !== "GET") {
    return; // never touch a write
  }
  var url = new URL(req.url);
  if (url.origin !== self.location.origin) {
    return; // only our own origin
  }

  // Navigations: network first, always. A page carries a balance, so it must
  // come from the server. Only when the network is unreachable do we fall back
  // to the static offline page -- never to cached tab data.
  if (req.mode === "navigate") {
    event.respondWith(
      fetch(req).catch(function () {
        return caches.match("/static/offline.html", { ignoreSearch: true });
      })
    );
    return;
  }

  // The static shell: cache first. ignoreSearch lets a "?v=<digest>" request
  // match the bare precached URL; the cache name is version-scoped, so the copy
  // returned is always the one this worker installed. No runtime put here -- the
  // shell is fully precached, and putting a versioned response beside the bare
  // one would let ignoreSearch return the wrong generation.
  if (url.pathname.indexOf("/static/") === 0) {
    event.respondWith(
      caches.match(req, { ignoreSearch: true }).then(function (hit) {
        return hit || fetch(req);
      })
    );
    return;
  }

  // Everything else -- avatars, htmx fragments, data reads -- goes straight to
  // the network, uncached. Falling through with no respondWith is that default.
});
