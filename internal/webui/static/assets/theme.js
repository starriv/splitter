// Apply the theme, layout and view before first paint. The stored theme
// wins, otherwise the system preference applies. Loaded synchronously from
// <head>. The view and layout are mirrored in CSS so the first paint matches
// the URL without waiting for the deferred page script.
(function () {
  var theme = null;
  try { theme = localStorage.getItem("splitter-theme"); } catch (e) {}
  if (theme !== "light" && theme !== "dark") {
    theme = matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  var root = document.documentElement;
  root.dataset.theme = theme;
  root.dataset.layout = matchMedia("(min-width: 960px)").matches ? "wide" : "narrow";
  // Keep the browser UI (mobile address bar, etc.) in step with the page before
  // first paint. The values mirror --background in app.css.
  var meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute("content", theme === "dark" ? "#141414" : "#ffffff");

  var params = new URLSearchParams(location.search);
  var view = params.get("view");
  if (!view && params.get("name")) view = "diagnose";
  var known = { overview: 1, rules: 1, upstreams: 1, sets: 1, diagnose: 1, queries: 1, observability: 1 };
  root.dataset.view = known[view] ? view : "overview";
})();
