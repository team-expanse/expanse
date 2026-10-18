"""ui-secrets (Phase 14 B): a db/postgres block deployed from the form with its superuser password
left empty (generated, shown once) and its replication password given. Neither is stored or
written anywhere in plain text; the replica gets them as systemd credentials, and postgres accepts
the generated one. A new password through the edit form restarts the replica with it.
Runs after cluster-common.py, block-common.py, vol_cluster.py, single_node_common.py and
ui_forms_common.py.
"""

import html

PASSWORD = "ui-secrets-test-password"
REPL = "given-replication-pass-4711"
NEW_SUPER = "changed-super-pass-0815"
UNIT = "expanse-block@default-pg-0.service"
CREDS = "/run/expanse/block-secrets/default-pg-0"
PSQL = "/nix/var/nix/gcroots/expanse/block-runtime/db-postgres/bin/psql"


def psql_ok(password):
    rc, _ = n1.execute(f"PGPASSWORD={shlex.quote(password)} {PSQL} -h 127.0.0.1 -p 15432 -U postgres -tA -c 'SELECT 1'")
    return rc == 0


def invocation():
    return n1.succeed(f"systemctl show -p InvocationID --value {UNIT}").strip()


form_single("secrets")
n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
csrf = ui_login()

with subtest("deploying shows the generated password once"):
    status, _, body = post("/deploy/db/postgres", {
        "name": "pg", "replicas": "1", "storage/pgdata/size": "256Mi", "cfg/replicationPassword": REPL,
    })
    assert status == "200", f"deploy: {status}\n{body}"
    found = re.search(r'<code class="secret-value">([^<]+)</code>', body)
    assert found, f"no generated password on the page:\n{body}"
    SUPER = html.unescape(found.group(1))
    assert REPL not in body, "the given password is shown"

with subtest("the replica starts with its secrets as credentials"):
    wait_for(lambda: n1.execute(f"systemctl is-active {UNIT}")[0] == 0, "the pg replica running", 180)
    assert n1.succeed(f"stat -c '%a %U' {CREDS}").strip() == "700 root", "credential dir is not root-only"
    assert n1.succeed(f"cat {CREDS}/superuserPassword") == SUPER
    wait_for(lambda: psql_ok(SUPER), "postgres to accept the generated password", 240)
    assert not psql_ok("wrong-password"), "postgres accepts any password"

with subtest("no password is stored or written in plain text"):
    for v in (SUPER, REPL):
        n1.fail(f"grep -rqaF {shlex.quote(v)} /persist/expanse /run/expanse/block-replica /var/lib/expanse")
        assert v not in n1.succeed("expanse ctl block get pg"), "block get shows a password"
        assert v not in n1.succeed(f"{CURL} https://n1:8443/blocks/default/pg"), "the block page shows a password"
        assert v not in n1.succeed(f"{CURL} https://n1:8443/blocks/default/pg/edit"), "the edit form shows a password"

with subtest("a new password through the edit form restarts the replica with it"):
    before = invocation()
    status, _, body = post("/blocks/default/pg/edit", {"name": "pg", "replicas": "1", "cfg/superuserPassword": NEW_SUPER})
    assert status == "303", f"edit: {status}\n{body}"
    wait_for(lambda: invocation() not in ("", before), "the replica to restart", 120)
    wait_for(lambda: n1.execute(f"cat {CREDS}/superuserPassword")[1] == NEW_SUPER, "the new credential", 60)
    assert n1.succeed(f"cat {CREDS}/replicationPassword") == REPL, "the untouched password changed"
    n1.fail(f"grep -rqaF {NEW_SUPER} /persist/expanse /run/expanse/block-replica")

print("UI-SECRETS DONE")
