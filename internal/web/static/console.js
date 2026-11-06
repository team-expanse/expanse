// Attaches the console page's terminal to the VM's serial console over a WebSocket.
(function () {
  "use strict";
  document.addEventListener("DOMContentLoaded", function () {
    var el = document.getElementById("console");
    var status = document.getElementById("console-status");
    if (!el || !window.Terminal) return;
    var term = new window.Terminal({ cursorBlink: true, scrollback: 5000, fontSize: 14 });
    var fit = new window.FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(el);
    fit.fit();
    window.addEventListener("resize", function () { fit.fit(); });

    var url = new URL(el.dataset.socket, window.location.href);
    url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    var ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";
    var encoder = new TextEncoder();
    ws.onopen = function () {
      status.textContent = "Connected";
      term.focus();
    };
    ws.onmessage = function (e) { term.write(new Uint8Array(e.data)); };
    ws.onclose = function (e) {
      status.textContent = e.reason ? "Closed: " + e.reason : "Closed";
      term.write("\r\n\x1b[2m[console closed" + (e.reason ? ": " + e.reason : "") + "; reload to reconnect]\x1b[0m\r\n");
    };
    term.onData(function (d) {
      if (ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(d));
    });
  });
})();
