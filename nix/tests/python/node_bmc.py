"""node-bmc: the agent reads the BMC's LAN address through /dev/ipmi0, and the node page and
`expanse ctl node inspect` show it. Runs after cluster-common.py, block-common.py, vol_cluster.py,
single_node_common.py and ui_forms_common.py; IPMI_SIM is ipmi_sim's command line."""

import subprocess
import tempfile

PASSWORD = "node-bmc-test-password"

bmc_state = tempfile.mkdtemp()
bmc = subprocess.Popen(IPMI_SIM + ["-s", bmc_state], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

form_single("bmc")
n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
csrf = ui_login()

with subtest("ipmitool is installed and reaches the BMC"):
    out = n1.execute("ipmitool lan print")[1]  # exits 1: ipmi_sim lacks some parameters, as real BMCs can
    assert "10.0.40.17" in out, f"ipmitool lan print: {out}"

with subtest("expanse ctl node inspect shows the BMC"):
    wait_for(lambda: "bmc:      10.0.40.17 d0:94:66:1a:2b:3c (Static Address)" in n1.succeed("expanse ctl node inspect"),
             "the BMC in the node inventory", 120)

with subtest("the node's Hardware tab links the BMC"):
    body = n1.succeed(f"{CURL} 'https://n1:8443/nodes/n1?tab=hardware'")
    for want in ("Management controller", 'href="https://10.0.40.17/"', "d0:94:66:1a:2b:3c", "Static Address"):
        assert want in body, f"the hardware tab lacks {want!r}"

bmc.terminate()
