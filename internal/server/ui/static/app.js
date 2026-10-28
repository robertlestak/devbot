// Keeps a page live: every few seconds (the main element's data-refresh)
// it fetches the page again and swaps in the new main, so the filters and
// scroll position stay put. It pauses while the tab is hidden, and reloads
// when the session has ended so the sign-in page shows.
(function () {
  "use strict";
  var main = document.querySelector("main[data-refresh]");
  if (!main || !window.fetch || !window.DOMParser) return;
  var every = Number(main.getAttribute("data-refresh")) * 1000;
  if (!(every > 0)) return;
  var busy = false;

  function refresh() {
    if (busy || document.hidden) return;
    busy = true;
    fetch(location.href, { credentials: "same-origin", cache: "no-store" })
      .then(function (r) {
        if (r.status === 401) {
          location.reload();
          return null;
        }
        return r.ok ? r.text() : null;
      })
      .then(function (html) {
        if (!html) return;
        var next = new DOMParser().parseFromString(html, "text/html").querySelector("main");
        if (!next) return;
        // Keep whatever the reader has opened.
        var open = Array.prototype.map.call(main.querySelectorAll("details[open][id]"), function (d) { return d.id; });
        main.replaceChildren.apply(main, Array.prototype.slice.call(next.childNodes));
        open.forEach(function (id) {
          var d = document.getElementById(id);
          if (d) d.open = true;
        });
      })
      .catch(function () {})
      .then(function () { busy = false; });
  }

  setInterval(refresh, every);
  document.addEventListener("visibilitychange", refresh);
})();
