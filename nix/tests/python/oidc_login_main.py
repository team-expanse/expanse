"""oidc-login: PHASE-10-TASKS.md Stream C (X3). A real dex OIDC provider
(dexidp/dex, nixpkgs `dex-oidc`) issues a login to the web UI, driven
hop by hop through dex's own real HTTP redirects -- never a mocked
token exchange -- ending in the exact same store-backed auth.Session
password login issues (proven by reusing the resulting cookie against a
*different* node, D2's VIP-transparency precedent).

Runs after cluster-common.py; NODES/n1/n2/n3/form/wait_quorum come from
that shared file. DEX_BIN/DEX_PASSWORD/DEX_PASSWORD_HASH are injected by
oidc-login.nix (a real store path and a build-time-computed bcrypt hash,
not hand-typed).
"""

CAFILE = "/persist/expanse/ca/ui-ca.pem"
COOKIES = "/root/oidc-cookies.txt"
CLIENT_SECRET = "vm-test-oidc-client-secret-do-not-log-me"
REDIRECT_URL = "https://n1:8443/login/oidc/callback"
ALLOWED_EMAIL = "alice@example.com"
UNAUTHORIZED_EMAIL = "mallory@example.com"

DEX_CONFIG = f"""
issuer: http://127.0.0.1:5556/dex
storage:
  type: memory
web:
  http: 127.0.0.1:5556
oauth2:
  skipApprovalScreen: true
staticClients:
- id: expanse-ui
  secret: {CLIENT_SECRET}
  redirectURIs:
  - '{REDIRECT_URL}'
  name: 'Expanse UI'
enablePasswordDB: true
staticPasswords:
- email: "{ALLOWED_EMAIL}"
  hash: "{DEX_PASSWORD_HASH}"
  username: "alice"
  userID: "08a8684b-db88-4b73-90a9-3cd1661f5466"
- email: "{UNAUTHORIZED_EMAIL}"
  hash: "{DEX_PASSWORD_HASH}"
  username: "mallory"
  userID: "6b3f6e2a-3b0a-4a9a-9a3a-2a6a9a3a2a6a"
"""


def curl_next_hop(m, url, extra=""):
    """Runs one real HTTP hop through curl (no -L: this drives dex's
    actual redirect chain one real request at a time, exactly as a
    browser would) and returns the Location header curl itself resolved
    -- '' if the response was not a redirect."""
    out = m.succeed(
        f"curl -s -o /dev/null -D /dev/null -w '%{{redirect_url}}' "
        f"-c {COOKIES} -b {COOKIES} --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        f"{extra} '{url}'"
    )
    return out.strip()


def curl_status(m, url, extra=""):
    out = m.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' "
        f"-c {COOKIES} -b {COOKIES} --resolve n1:8443:127.0.0.1 --cacert {CAFILE} "
        f"{extra} '{url}'"
    )
    return out.strip()


def run_login(m, email, password):
    """Drives one full authorization-code round trip against the real
    dex instance for email/password, returning the final HTTP status
    code the web UI's own /login/oidc/callback answered with. Each hop
    is dex's own real redirect (Location header), not a URL this test
    predicts -- only the final POST target's shape (whatever /start
    ultimately lands on) is assumed to accept a login/password form,
    which is dex's real local-connector login page."""
    m.succeed(f"rm -f {COOKIES}")
    url = curl_next_hop(m, "https://n1:8443/login/oidc/start")
    assert url, "GET /login/oidc/start did not redirect to the IdP"
    # dex: /auth -> /auth/local (connector selection, skipped -- only
    # one connector is configured) -> /auth/local/login (the real login
    # form's GET, whose own URL is also its POST target).
    url = curl_next_hop(m, url)
    assert url, f"dex did not redirect from the authorization endpoint: {url}"
    url = curl_next_hop(m, url)
    assert url, "dex did not redirect to its local-connector login form"

    login_url = url
    final = curl_next_hop(m, login_url, extra=f"-X POST --data 'login={email}&password={password}'")
    assert final, "dex did not redirect back to the configured redirect_uri after login"
    assert final.startswith(REDIRECT_URL), f"dex redirected to {final!r}, not our redirect_uri"

    return curl_status(m, final)


form("oidc")

with subtest("start a real dex instance (memory storage, one static client, two static users)"):
    n1.succeed(f"cat > /root/dex-config.yaml <<'EOF'\n{DEX_CONFIG}\nEOF")
    n1.succeed(f"nohup {DEX_BIN} serve /root/dex-config.yaml > /root/dex.log 2>&1 &")
    n1.wait_for_open_port(5556)

with subtest("baseline: password login still works -- OIDC is additive, not a replacement (D2)"):
    n1.succeed("expanse ctl admin reset-password --password 'vm-test-admin-password-do-not-log-me'")
    code = n1.succeed(
        "curl -s -o /dev/null -w '%{http_code}' --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} -d 'username=admin&password=vm-test-admin-password-do-not-log-me' "
        "https://n1:8443/login"
    ).strip()
    assert code == "303", f"password login returned {code}, want 303"

with subtest("login page has no SSO button before OIDC is configured"):
    out = n1.succeed(f"curl -sf --resolve n1:8443:127.0.0.1 --cacert {CAFILE} https://n1:8443/login")
    assert "/login/oidc/start" not in out, "SSO button present before OIDC was configured"

with subtest("configure OIDC login against the real dex instance"):
    out = n1.succeed(
        "expanse ctl oidc configure "
        "--issuer http://127.0.0.1:5556/dex "
        "--client-id expanse-ui "
        f"--client-secret '{CLIENT_SECRET}' "
        f"--redirect-url {REDIRECT_URL} "
        f"--allow-email {ALLOWED_EMAIL}"
    )
    assert "configured" in out, f"unexpected oidc configure output: {out}"

with subtest("login page now offers SSO"):
    out = n1.succeed(f"curl -sf --resolve n1:8443:127.0.0.1 --cacert {CAFILE} https://n1:8443/login")
    assert "/login/oidc/start" in out, "SSO button missing after OIDC was configured"

with subtest("a real end-to-end dex login for the allow-listed email succeeds"):
    status = run_login(n1, ALLOWED_EMAIL, DEX_PASSWORD)
    assert status == "303", f"OIDC callback returned {status}, want 303"

    out = n1.succeed(f"curl -sf -b {COOKIES} --resolve n1:8443:127.0.0.1 --cacert {CAFILE} https://n1:8443/")
    assert "n1" in out, f"the OIDC-issued session did not authorize a follow-up request: {out}"

with subtest("the OIDC-issued session is honored on a different node (store-backed, VIP-transparent)"):
    # The session record lives in the Raft store, not process memory
    # (internal/web/auth's own doc comment) -- so the *value* n1 issued
    # must authorize a request a completely different node's own :8443
    # listener answers, exactly like password login's session does
    # (ROADMAP.md Phase 2 A3). Extract the raw cookie value from n1's
    # jar (Netscape format: ...\tname\tvalue) and present it to n2.
    session_value = n1.succeed(
        f"grep expanse_session {COOKIES} | awk -F'\\t' '{{print $NF}}'"
    ).strip()
    assert session_value, f"could not extract the session cookie from {COOKIES}"
    out = n2.succeed(
        f"curl -sf -H 'Cookie: expanse_session={session_value}' "
        f"--resolve n2:8443:127.0.0.1 --cacert {CAFILE} https://n2:8443/"
    )
    assert "n2" in out, f"n2 did not honor the session n1's OIDC login issued: {out}"

with subtest("a real dex login for an email NOT on the allow-list is refused"):
    status = run_login(n1, UNAUTHORIZED_EMAIL, DEX_PASSWORD)
    assert status == "401", f"OIDC callback for an unauthorized email returned {status}, want 401"

    code = n1.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -b {COOKIES} --resolve n1:8443:127.0.0.1 "
        f"--cacert {CAFILE} https://n1:8443/"
    ).strip()
    assert code == "303", f"an unauthorized OIDC login must not authorize a follow-up request, got {code}"

print("OIDC-LOGIN DONE")
