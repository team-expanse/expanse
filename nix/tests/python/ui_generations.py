"""ui-generations: a node's NixOS system generations on its page, with each one's release, drift
and a keep-newest cleanup; and an idle node's Activity feed staying quiet.

Generations 1-3 are hand-made systems running this node's expanse; 4 is the real system, recorded
as the one an upgrade built for the release. So 4 is a release build and 1-3 are "modified".
Runs after cluster-common.py, block-common.py, vol_cluster.py, single_node_common.py and
ui_forms_common.py.
"""

PASSWORD = "ui-generations-test-password"
PROFILE = "/nix/var/nix/profiles/system"


def page(path):
    return n1.succeed(f"{CURL} https://n1:8443{path}")


def report():
    out = kv(n1, "get /os-generations/n1")[1]
    try:
        return json.loads(out)
    except ValueError:
        return {}


def numbers():
    return [g["n"] for g in report().get("generations", [])]


form_single("generations")
n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
csrf = ui_login()

with subtest("the node publishes its generations with their releases"):
    system = n1.succeed("readlink -f /run/current-system").strip()
    expanse = n1.succeed(f"readlink -f {system}/sw/bin/expanse").strip()
    version = expanse.split("/")[3].split("-expanse-", 1)[1]  # /nix/store/<hash>-expanse-<version>/bin/expanse
    for i in (1, 2, 3):
        fake = n1.succeed(
            f"d=$(mktemp -d)/fake-system-{i} && mkdir -p $d/sw/bin && ln -s {expanse} $d/sw/bin/expanse"
            f" && echo {i} > $d/id && nix-store --add $d"
        ).strip()
        n1.succeed(f"nix-env -p {PROFILE} --set {fake}")
    n1.succeed(f"nix-env -p {PROFILE} --set {system}")
    n1.succeed(f"mkdir -p /persist/expanse && printf '{{\"%s\": \"%s\"}}' {system} {version} > /persist/expanse/release-systems.json")
    wait_for(lambda: numbers() == [4, 3, 2, 1], "the four generations reported, newest first", 60)
    gens = {g["n"]: g for g in report()["generations"]}
    assert gens[4].get("exact") and gens[4].get("current"), f"generation 4: {gens[4]}"
    assert re.search(r"quorum:\s+1/1", status(n1)), "the generations report reads as an extra node"
    assert gens[1]["release"] == version and not gens[1].get("exact") and gens[1].get("recorded"), f"generation 1: {gens[1]}"

with subtest("the node page shows each generation's release and the running one"):
    body = page("/nodes/n1?tab=generations")
    for want in ("Release build", f"{version}, modified", "Running", 'name="keep"'):
        assert want in body, f"the generations tab lacks {want!r}"
    assert version in page("/nodes/n1"), "the node header does not show the running release"

with subtest("cleanup keeps the newest and the running generation"):
    status, loc, body = post("/nodes/n1/generations", {"keep": "1"})
    assert status == "303", f"clean: {status}\n{body}"
    wait_for(lambda: numbers() == [4], "generations 1-3 removed", 120)
    clean = report().get("clean") or {}
    assert sorted(clean.get("removed", [])) == [1, 2, 3] and not clean.get("error"), f"clean result: {clean}"
    n1.fail(f"test -e {PROFILE}-1-link")
    n1.succeed(f"grep -qx {system} /run/boot-installs")  # the boot menu was rewritten without them
    assert "Removed 3 generations" in page("/nodes/n1?tab=generations"), "the page does not show the cleanup"

with subtest("an idle node's Activity shows no lease renewals or status rewrites"):
    time.sleep(15)  # let the leases and reconciler settle
    before = page("/events")
    time.sleep(20)
    after = page("/events")
    for marker in ("/leases/", "/status/resources/", "/reconcile"):
        assert after.count(marker) == before.count(marker), f"Activity grew {marker} entries while idle"

print("UI-GENERATIONS DONE")
