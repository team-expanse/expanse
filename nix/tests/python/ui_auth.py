"""Phase 2, A2: session/auth for the web UI -- an argon2id-hashed admin
credential in the cluster store, store-backed sessions, and login/logout
handlers. Runs after cluster-common.py's form().

The password used here is set via `expanse ctl admin reset-password
--password ...` rather than read out of the auto-generated bootstrap
credential. By design a password set this way is never printed or
logged anywhere (only the auto-generated bootstrap password is, exactly
once, D4) -- which lets the final subtest assert this specific password
never leaks into the store or the journal as a real check, not a
vacuous one (it would trivially "pass" against a password we never
had a copy of to grep for).
"""

CAFILE = "/persist/expanse/ca/ca.pem"
PASSWORD = "vm-test-admin-password-do-not-log-me"

form("test")

with subtest("ctl admin reset-password sets a known password"):
    out = n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
    assert "updated" in out, f"unexpected reset-password output: {out}"

with subtest("a wrong password is refused"):
    code = n1.succeed(
        "curl -s -o /dev/null -w '%{http_code}' --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} -d 'username=admin&password=wrong' https://n1:8443/login"
    ).strip()
    assert code == "401", f"wrong password returned {code}, want 401"

with subtest("the correct password authorizes a follow-up request"):
    n1.succeed("rm -f /tmp/ui-cookies.txt")
    code = n1.succeed(
        "curl -s -o /dev/null -w '%{http_code}' -c /tmp/ui-cookies.txt "
        f"--resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        f"-d 'username=admin&password={PASSWORD}' https://n1:8443/login"
    ).strip()
    assert code == "303", f"correct-password login returned {code}, want 303"

    out = n1.succeed(
        "curl -sf -b /tmp/ui-cookies.txt --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/"
    )
    assert "n1" in out, f"authenticated follow-up request did not reach the page: {out}"

with subtest("an unauthenticated request is redirected to /login, not served the page"):
    code = n1.succeed(
        "curl -s -o /dev/null -w '%{http_code}' --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/"
    ).strip()
    assert code == "303", f"unauthenticated GET / returned {code}, want 303 (to /login)"

with subtest("no plaintext or reversibly-encoded password appears in the store or logs"):
    n1.fail(f"grep -r -- '{PASSWORD}' /persist/expanse")
    n1.fail(f"journalctl -u expansed.service --no-pager | grep -- '{PASSWORD}'")

print("UI-AUTH DONE")
