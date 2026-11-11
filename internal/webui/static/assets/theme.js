// Apply the theme before first paint: the stored preference wins,
// otherwise the system preference applies. Loaded synchronously from
// <head>.
(function () {
  var theme = null;
  try { theme = localStorage.getItem("splitter-theme"); } catch (e) {}
  if (theme !== "light" && theme !== "dark") {
    theme = matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  document.documentElement.dataset.theme = theme;
})();
