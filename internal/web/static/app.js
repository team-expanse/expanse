// Behaviour the shell needs beyond htmx: the theme toggle, the phone
// nav drawer, toasts (from HX-Trigger headers and request errors),
// <dialog>-based confirms, auto-submitting filters and log autoscroll.
// No inline script anywhere (CSP script-src 'self').
(function () {
  "use strict";

  // ---- theme -----------------------------------------------------------
  function currentTheme() {
    var forced = document.documentElement.getAttribute("data-theme");
    if (forced) return forced;
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  function setTheme(theme) {
    document.documentElement.setAttribute("data-theme", theme);
    try { localStorage.setItem("expanse-theme", theme); } catch (e) {}
  }
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-theme-toggle]");
    if (btn) setTheme(currentTheme() === "dark" ? "light" : "dark");
  });

  // ---- phone nav drawer ------------------------------------------------
  function setNav(open) {
    document.body.classList.toggle("nav-open", open);
    var scrim = document.querySelector(".scrim");
    if (scrim) scrim.hidden = !open;
    var btn = document.querySelector("[data-nav-toggle]");
    if (btn) btn.setAttribute("aria-expanded", open ? "true" : "false");
  }
  document.addEventListener("click", function (ev) {
    if (ev.target.closest("[data-nav-toggle]")) setNav(!document.body.classList.contains("nav-open"));
    else if (ev.target.closest("[data-nav-close]")) setNav(false);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") setNav(false);
  });

  // ---- toasts ----------------------------------------------------------
  var toastTimers = new WeakMap();
  function toast(kind, text) {
    var host = document.getElementById("toasts");
    if (!host) return;
    var el = document.createElement("div");
    el.className = "toast toast-" + (kind || "info");
    el.setAttribute("role", "status");
    var span = document.createElement("span");
    span.textContent = text;
    var close = document.createElement("button");
    close.className = "toast-close";
    close.type = "button";
    close.setAttribute("aria-label", "Dismiss");
    close.textContent = "×";
    el.appendChild(span);
    el.appendChild(close);
    host.appendChild(el);
    armToast(el);
  }
  function armToast(el) {
    toastTimers.set(el, setTimeout(function () { dismissToast(el); }, el.classList.contains("toast-error") ? 12000 : 6000));
  }
  function dismissToast(el) {
    clearTimeout(toastTimers.get(el));
    el.classList.add("toast-leaving");
    setTimeout(function () { el.remove(); }, 200);
  }
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest(".toast-close");
    if (btn) dismissToast(btn.parentElement);
  });
  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("#toasts .toast").forEach(armToast);
  });
  // HX-Trigger: {"toast": {"kind": "...", "text": "..."}}
  document.body.addEventListener("toast", function (ev) {
    var d = ev.detail || {};
    toast(d.kind, d.text);
  });
  document.body.addEventListener("htmx:responseError", function (ev) {
    var xhr = ev.detail.xhr;
    var text = (xhr && xhr.responseText || "").trim();
    if (text.charAt(0) === "<" || text.length > 300) text = xhr.status + " " + xhr.statusText;
    toast("error", text || "Request failed");
  });
  document.body.addEventListener("htmx:sendError", function () {
    toast("error", "Could not reach the server");
  });
  // A form marked data-reset clears after a successful htmx submit.
  document.body.addEventListener("htmx:afterRequest", function (ev) {
    var form = ev.target.closest && ev.target.closest("form[data-reset]");
    if (form && ev.detail.successful) form.reset();
  });

  // ---- live indicator --------------------------------------------------
  function setLive(state) {
    document.querySelectorAll("[data-live]").forEach(function (el) {
      el.classList.toggle("live-off", state !== "open");
      el.title = state === "open" ? "Updates arrive over a live connection" : "Live connection lost; reconnecting";
    });
  }
  document.body.addEventListener("htmx:sseOpen", function () { setLive("open"); });
  document.body.addEventListener("htmx:sseError", function () { setLive("error"); });
  document.body.addEventListener("htmx:sseClose", function () { setLive("closed"); });

  // ---- confirm dialogs -------------------------------------------------
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-confirm]");
    if (!btn) return;
    var dlg = document.getElementById(btn.getAttribute("data-confirm"));
    if (dlg && typeof dlg.showModal === "function") dlg.showModal();
  });
  document.addEventListener("close", function (ev) {
    var dlg = ev.target;
    if (!(dlg instanceof HTMLDialogElement) || dlg.returnValue !== "confirm") return;
    var form = document.querySelector(dlg.getAttribute("data-submit") || "");
    if (form) form.requestSubmit();
    dlg.returnValue = "";
  }, true);
  // Click on the backdrop closes the dialog.
  document.addEventListener("click", function (ev) {
    if (ev.target instanceof HTMLDialogElement && ev.target.open) ev.target.close("cancel");
  });

  // ---- small conveniences ----------------------------------------------
  document.addEventListener("change", function (ev) {
    var el = ev.target.closest("[data-autosubmit]");
    if (el && el.form) el.form.requestSubmit();
  });
  document.body.addEventListener("htmx:sseMessage", function (ev) {
    var pre = ev.target.closest && ev.target.closest("[data-autoscroll]");
    if (pre && pre.scrollHeight - pre.scrollTop - pre.clientHeight < 80) pre.scrollTop = pre.scrollHeight;
    var hide = document.querySelector("[data-hide-on-rows]");
    if (hide) hide.hidden = true;
  });
})();
