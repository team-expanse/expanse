// Run with node --require <preload>: Uptime Kuma's own synchronous = NORMAL must come out as FULL (2).
"use strict";
const assert = require("assert");
const os = require("os");
const path = require("path");
const sqlite3 = require("@louislam/sqlite3");

const db = new sqlite3.Database(path.join(os.tmpdir(), `sync-full-${process.pid}.db`));
const level = (cb) => db.get("PRAGMA synchronous", (err, row) => cb(err, row && row.synchronous));

db.run("PRAGMA synchronous = NORMAL", (err) => {
    assert.ifError(err);
    level((err, run) => {
        assert.ifError(err);
        assert.strictEqual(run, 2, `run: synchronous is ${run}, want 2 (FULL)`);
        db.exec("PRAGMA synchronous = OFF", (err) => {
            assert.ifError(err);
            level((err, exec) => {
                assert.ifError(err);
                assert.strictEqual(exec, 2, `exec: synchronous is ${exec}, want 2 (FULL)`);
                console.log("uptime-kuma preload: synchronous stays FULL");
            });
        });
    });
});
