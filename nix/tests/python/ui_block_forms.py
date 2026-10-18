"""ui-block-forms (Phase 14 D): a block deployed and edited through the web UI's forms alone.

On a one-node cluster, web/nginx is deployed from /deploy with only a name and a server name
(everything else comes from the type's template and defaults), serves on its VIP to an outside
client, then has its listen port changed through the edit form and still serves: the VIP's
forwarding followed the port. Runs after cluster-common.py, block-common.py, vol_cluster.py and
single_node_common.py; n9 is the external client.
"""

PASSWORD = "ui-forms-test-password"


def block_vip():
    out = n1.execute("expanse ctl kv --socket /run/expanse/agent.sock get /network/vipPool/external/default/site 2>&1")[1]
    try:
        return json.loads(out)["addr"].split("/")[0]
    except (ValueError, KeyError):
        return ""


def vip_serves():
    vip = block_vip()
    out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")[1]
    return vip != "" and out.strip() == "200"


form_single("forms")
n9.start()
n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
csrf = ui_login()

with subtest("the catalog page and the nginx form render"):
    page = n1.succeed(f"{CURL} https://n1:8443/deploy")
    assert 'href="/deploy/web/nginx"' in page, "nginx missing from /deploy"
    page = n1.succeed(f"{CURL} https://n1:8443/deploy/web/nginx")
    assert 'name="cfg/serverName"' in page, "the nginx form has no serverName field"

with subtest("a bad name is refused on its field and creates nothing"):
    status, _, body = post("/deploy/web/nginx", {"name": "Bad_Name", "cfg/serverName": "site.test"})
    assert status == "400", f"status {status}"
    assert "DNS-1123" in body, "the name error is not shown"
    n1.fail("expanse ctl block get Bad_Name")

with subtest("nginx deployed from the form serves on its VIP"):
    status, loc, body = post("/deploy/web/nginx", {"name": "site", "cfg/serverName": "site.test"})
    assert status == "303" and loc == "/blocks/default/site", f"deploy: {status} {loc}\n{body[-2000:]}"
    b = wait_phase(n1, "site", ["RUNNING"], 180)
    assert b["spec"]["replicas"] == 1, f"replicas not fitted to one node: {b['spec']}"
    wait_for(vip_serves, "the client to reach nginx on the VIP", 90)

with subtest("changing the port through the edit form keeps the VIP serving"):
    status, loc, body = post("/blocks/default/site/edit", {
        "name": "site", "replicas": "1", "cfg/serverName": "site.test", "cfg/port": "8090",
    })
    assert status == "303", f"edit: {status}\n{body[-2000:]}"
    wait_for(lambda: get_json(n1, "site")["spec"]["network"]["ports"][0].get("targetPort") == 8090,
             "the port's target to follow the config", 30)
    wait_for(lambda: n1.execute("ss -ltn | grep -q ':8090 '")[0] == 0, "nginx to listen on 8090", 180)
    wait_for(vip_serves, "the client to reach nginx on the VIP after the edit", 120)

print("UI-BLOCK-FORMS PASSED: deploy and edit from the web UI's forms, served on the VIP")
