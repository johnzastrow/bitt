// Live size counters for length-bounded fields.
//
// The content-security-policy is script-src 'self', so this cannot be inline;
// it is a file for that reason rather than by preference. It is also entirely
// progressive: the server renders the current size into the counter element, so
// a page with scripting off still shows a real number and the server still
// enforces the bound. This only keeps that number honest while typing.
//
// A field opts in with three attributes:
//
//   data-counter  the id of the element to write the size into
//   data-limit    the maximum, in bytes
//   data-warn     the size at which to caution (optional)
//   data-unit     "bytes" or "characters", for the wording
//
// Sizes are counted in BYTES, not characters, because that is what the limits
// are: ntfy measures a message in bytes, so an emoji or an accented name costs
// more than one. Counting characters here would tell a Provider their message
// fits when the send is about to refuse it.
(function () {
  "use strict";

  var encoder = window.TextEncoder ? new TextEncoder() : null;

  function size(value) {
    if (encoder) {
      return encoder.encode(value).length;
    }
    // No TextEncoder: count UTF-8 bytes by hand rather than silently
    // undercounting with .length.
    return unescape(encodeURIComponent(value)).length;
  }

  function group(n) {
    return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  }

  function update(field) {
    var out = document.getElementById(field.getAttribute("data-counter"));
    if (!out) {
      return;
    }
    var limit = parseInt(field.getAttribute("data-limit"), 10) || 0;
    var warn = parseInt(field.getAttribute("data-warn"), 10) || 0;
    var unit = field.getAttribute("data-unit") || "bytes";
    var used = size(field.value);

    out.textContent = group(used) + " of " + group(limit) + " " + unit;
    out.classList.toggle("over", limit > 0 && used > limit);
    out.classList.toggle("warn", warn > 0 && used > warn && used <= limit);
  }

  function wire() {
    var fields = document.querySelectorAll("[data-counter]");
    for (var i = 0; i < fields.length; i++) {
      (function (field) {
        update(field);
        field.addEventListener("input", function () {
          update(field);
        });
      })(fields[i]);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wire);
  } else {
    wire();
  }
})();
