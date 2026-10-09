// Loaded with node --require: Uptime Kuma hard-codes PRAGMA synchronous = NORMAL, under which a crash loses
// committed WAL frames, so every synchronous pragma its SQLite driver runs becomes FULL.
"use strict";
const Module = require("module");
const SET_SYNC = /^\s*PRAGMA\s+synchronous\s*[=(]/i;
const FULL = "PRAGMA synchronous = FULL";
const load = Module._load;
let patched = false;

function forceFull(method) {
    return function (sql, ...rest) {
        if (typeof sql === "string" && SET_SYNC.test(sql)) {
            sql = FULL;
        }
        return method.call(this, sql, ...rest);
    };
}

Module._load = function (request, ...rest) {
    const mod = load.call(this, request, ...rest);
    if (request === "@louislam/sqlite3" && !patched) {
        patched = true;
        mod.Database.prototype.run = forceFull(mod.Database.prototype.run);
        mod.Database.prototype.exec = forceFull(mod.Database.prototype.exec);
        console.error("expanse: SQLite synchronous pragmas forced to FULL");
    }
    return mod;
};
