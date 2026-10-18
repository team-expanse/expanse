"""Web UI form helpers for one-node UI tests: log in with a cookie jar and post forms.

Runs after single_node_common.py; the including test sets PASSWORD before ui_login() and assigns
its result to the global csrf.
"""

import shlex

CAFILE = "/persist/expanse/ca/ui-ca.pem"
JAR = "/tmp/ui-cookies.txt"
CURL = f"curl -s -b {JAR} --resolve n1:8443:127.0.0.1 --cacert {CAFILE}"


def ui_login():
    n1.succeed(f"rm -f {JAR}")
    code = n1.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -c {JAR} --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} -d 'username=admin&password={PASSWORD}' https://n1:8443/login"
    ).strip()
    assert code == "303", f"login returned {code}, want 303"
    token = n1.succeed(f"awk -F'\\t' '$6==\"expanse_csrf\"{{print $7}}' {JAR}").strip()
    assert token, "no expanse_csrf cookie"
    return token


def post(path, fields):
    """POSTs the form fields; returns (status, Location header, body)."""
    args = " ".join(f"--data-urlencode {shlex.quote(k + '=' + v)}" for k, v in fields.items())
    out = n1.succeed(
        f"{CURL} -o /tmp/post-body.html -D - -H 'X-CSRF-Token: {csrf}' {args} https://n1:8443{path}"
    )
    status = out.splitlines()[0].split()[1]
    loc = next((ln.split(":", 1)[1].strip() for ln in out.splitlines() if ln.lower().startswith("location:")), "")
    return status, loc, n1.succeed("cat /tmp/post-body.html")
