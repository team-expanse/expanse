// Applies the stored theme before first paint so a dark-mode user never
// sees a light flash. Loaded synchronously in <head>; everything else is
// deferred. localStorage may be unavailable (private mode, blocked site
// data), so every access is guarded.
(function () {
  var theme = null;
  try { theme = localStorage.getItem("expanse-theme"); } catch (e) {}
  if (theme === "light" || theme === "dark") {
    document.documentElement.setAttribute("data-theme", theme);
  }
})();
