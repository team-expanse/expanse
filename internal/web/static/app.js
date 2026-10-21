// Behaviour the shell needs beyond htmx: the theme toggle, closing pop-up
// menus, toasts (from HX-Trigger headers and request errors),
// <dialog>-based confirms, auto-submitting filters, log autoscroll and
// bounded live lists.
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

  // ---- pop-up menus (account, phone "More") ----------------------------
  // An open <details> menu closes on a click outside it or on Escape.
  function closeMenus(except) {
    document.querySelectorAll("details.menu[open], details.more[open]").forEach(function (d) {
      if (d !== except) d.open = false;
    });
  }
  document.addEventListener("click", function (ev) {
    closeMenus(ev.target.closest("details.menu, details.more"));
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeMenus(null);
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
  document.body.addEventListener("htmx:sseOpen", function (ev) {
    setLive("open");
    // The server replays the log tail on every (re)connect.
    ev.target.querySelectorAll("[data-clear-on-open]").forEach(function (el) { el.replaceChildren(); });
  });
  document.body.addEventListener("htmx:sseError", function (ev) {
    setLive("error");
    // A closed stream may mean the session expired: ask as htmx does, and
    // follow the server to /login rather than retrying forever.
    var src = ev.detail && ev.detail.source;
    if (!src || src.readyState !== EventSource.CLOSED) return;
    fetch(window.location.href, { headers: { "HX-Request": "true" } }).then(function (res) {
      if (res.status === 401) window.location.assign(res.headers.get("HX-Redirect") || "/login");
    }).catch(function () {});
  });
  document.body.addEventListener("htmx:sseClose", function () { setLive("closed"); });

  // ---- live swaps wait for the user -------------------------------------
  // A swap would close an open confirm or reset a field being edited, so
  // keep the newest update for that region until the user is done.
  var held = new Map();
  function busy(region) {
    var el = document.activeElement;
    return region.querySelector("dialog[open]") ||
      (el && el !== document.body && region.contains(el) && el.matches("input, select, textarea"));
  }
  document.body.addEventListener("htmx:sseBeforeMessage", function (ev) {
    if (!busy(ev.target)) return;
    ev.preventDefault();
    held.set(ev.target, ev.detail.data);
  });
  function release() {
    held.forEach(function (data, region) {
      if (busy(region)) return;
      held.delete(region);
      if (document.body.contains(region)) htmx.swap(region, data, { swapStyle: "innerHTML" });
    });
  }
  document.addEventListener("close", function () { setTimeout(release, 0); }, true);
  document.addEventListener("focusout", function () { setTimeout(release, 0); });

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
  // Click on the backdrop closes the dialog. The dialog's own padding is
  // also the dialog element, so only a click outside its box counts.
  document.addEventListener("click", function (ev) {
    var dlg = ev.target;
    if (!(dlg instanceof HTMLDialogElement) || !dlg.open) return;
    var box = dlg.getBoundingClientRect();
    var inside = ev.clientX >= box.left && ev.clientX <= box.right && ev.clientY >= box.top && ev.clientY <= box.bottom;
    if (!inside) dlg.close("cancel");
  });

  // ---- one submit at a time --------------------------------------------
  // A second click while the first request runs would report a false
  // "already exists" or act twice.
  function isHtmxForm(form) {
    return form.hasAttribute("hx-post") || form.hasAttribute("hx-put") || form.hasAttribute("hx-delete");
  }
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    if (!(form instanceof HTMLFormElement) || isHtmxForm(form) || form.method !== "post") return;
    if (form.dataset.submitting) { ev.preventDefault(); return; }
    // Set after this event, so a handler that cancels the submit leaves the form usable.
    setTimeout(function () { if (!ev.defaultPrevented) form.dataset.submitting = "1"; }, 0);
  });
  window.addEventListener("pageshow", function () {
    document.querySelectorAll("form[data-submitting]").forEach(function (f) { delete f.dataset.submitting; });
  });
  function setBusy(form, busy) {
    form.querySelectorAll("button[type=submit], button:not([type])").forEach(function (b) { b.disabled = busy; });
  }
  document.body.addEventListener("htmx:beforeRequest", function (ev) {
    if (ev.target instanceof HTMLFormElement) setBusy(ev.target, true);
  });
  document.body.addEventListener("htmx:afterRequest", function (ev) {
    if (ev.target instanceof HTMLFormElement) setBusy(ev.target, false);
  });

  // ---- small conveniences ----------------------------------------------
  document.addEventListener("change", function (ev) {
    var el = ev.target.closest("[data-autosubmit]");
    if (el && el.form) el.form.requestSubmit();
  });
  // A preset select shows its group's own fields only for "custom".
  function syncPreset(sel) {
    var custom = sel.closest("fieldset").querySelector("[data-preset-custom]");
    if (custom) custom.hidden = sel.value !== "custom";
  }
  document.querySelectorAll("select[data-preset]").forEach(syncPreset);
  document.addEventListener("change", function (ev) {
    if (ev.target.matches("select[data-preset]")) syncPreset(ev.target);
  });
  document.body.addEventListener("htmx:sseMessage", function (ev) {
    var pre = ev.target.closest && ev.target.closest("[data-autoscroll]");
    if (pre && pre.scrollHeight - pre.scrollTop - pre.clientHeight < 80) pre.scrollTop = pre.scrollHeight;
    // Keep live lists bounded, dropping the oldest end of the swap.
    var list = ev.target.closest && ev.target.closest("[data-live-limit]");
    if (list) {
      var limit = parseInt(list.getAttribute("data-live-limit"), 10);
      var newestFirst = list.getAttribute("hx-swap") === "afterbegin";
      while (list.children.length > limit) list.removeChild(newestFirst ? list.lastElementChild : list.firstElementChild);
    }
    var hide = document.querySelector("[data-hide-on-rows]");
    if (hide) hide.hidden = true;
  });
})();
