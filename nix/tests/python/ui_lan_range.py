"""ui-lan-range (Phase 14 H): the LAN address range set, changed and cleared from Settings.

The node's Nix config sets no range, so a block deployed from the form starts with a
cluster-internal address. Saving a range in Cluster settings moves the block (and the UI) onto the LAN,
where n9 reaches them; changing the range moves them again; clearing it takes them off the LAN.
Runs after cluster-common.py, block-common.py, vol_cluster.py, single_node_common.py and
ui_forms_common.py.
"""

PASSWORD = "ui-lan-range-test-password"


def vip_of(ref, scope):
    out = n1.execute(f"expanse ctl kv --socket /run/expanse/agent.sock get /network/vipPool/{scope}/{ref} 2>&1")[1]
    try:
        return json.loads(out)["addr"].split("/")[0]
    except (ValueError, KeyError):
        return ""


def reaches(url):
    return n9.execute(f"curl -sk -o /dev/null -w '%{{http_code}}' --connect-timeout 3 {url} || true")[1].strip()


def on_lan(first, last):
    """The block and the UI both hold LAN addresses in [first, last] and n9 reaches both."""
    lo, hi = int(first.split(".")[-1]), int(last.split(".")[-1])
    for ref in ("default/site", "expanse-ui"):
        addr = vip_of(ref, "external")
        if not addr.startswith("192.168.1.") or not lo <= int(addr.split(".")[-1]) <= hi:
            return False
        if vip_of(ref, "internal"):
            return False
    return (reaches(f"http://{vip_of('default/site', 'external')}/") == "200"
            and reaches(f"https://{vip_of('expanse-ui', 'external')}:8443/login") == "200")


form_single("lanrange")
n9.start()
n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
csrf = ui_login()

with subtest("without a range, a deployed block is cluster-internal and the UI says so"):
    page = n1.succeed(f"{CURL} https://n1:8443/deploy/web/nginx")
    assert "inside the cluster only" in page, "the deploy form does not warn about the missing range"
    status, loc, body = post("/deploy/web/nginx", {"name": "site", "cfg/serverName": "site.test"})
    assert status == "303", f"deploy: {status}\n{body[-2000:]}"
    wait_phase(n1, "site", ["RUNNING"], 180)
    wait_for(lambda: vip_of("default/site", "internal").startswith("10.43."), "an internal address", 30)
    page = n1.succeed(f"{CURL} https://n1:8443/blocks/default/site")
    assert "Cluster only" in page and vip_of("default/site", "internal") in page, "detail lacks the address"

with subtest("a range off the nodes' subnet is refused"):
    status, _, body = post("/cluster/network", {"externalVIPPool": "10.9.0.5-10.9.0.9"})
    assert status == "400" and "No node is on 10.9.0.0/24" in body, f"{status}\n{body[-2000:]}"

with subtest("saving a range moves the block and the UI onto the LAN"):
    status, loc, body = post("/cluster/network", {"externalVIPPool": "192.168.1.100-192.168.1.105"})
    assert status == "303", f"save: {status}\n{body[-2000:]}"
    wait_for(lambda: on_lan("192.168.1.100", "192.168.1.105"), "the block and UI on 192.168.1.100-105", 90)
    page = n1.succeed(f"{CURL} https://n1:8443/blocks/default/site")
    assert vip_of("default/site", "external") in page and "Cluster only" not in page, "detail lacks the LAN address"

with subtest("changing the range moves them again and frees the old addresses"):
    old = vip_of("default/site", "external")
    status, _, body = post("/cluster/network", {"externalVIPPool": "192.168.1.150-192.168.1.155"})
    assert status == "303", f"change: {status}\n{body[-2000:]}"
    wait_for(lambda: on_lan("192.168.1.150", "192.168.1.155"), "the block and UI on 192.168.1.150-155", 90)
    wait_for(lambda: reaches(f"http://{old}/") == "000", f"the old address {old} to stop answering", 30)

with subtest("clearing the range takes the block off the LAN"):
    lan = vip_of("default/site", "external")
    status, _, body = post("/cluster/network", {"externalVIPPool": ""})
    assert status == "303", f"clear: {status}\n{body[-2000:]}"
    wait_for(lambda: vip_of("default/site", "internal").startswith("10.43.") and not vip_of("default/site", "external"),
             "the block back on an internal address", 60)
    wait_for(lambda: reaches(f"http://{lan}/") == "000", f"{lan} to stop answering", 30)

print("UI-LAN-RANGE PASSED: the LAN range set, changed and cleared from Settings")
