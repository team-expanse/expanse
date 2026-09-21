resource vol-a1 {
  device /dev/drbd3 minor 3;
  disk /dev/vg0/vol-a1;
  meta-disk internal;
  net {
    protocol C;
    verify-alg sha1;
    after-sb-0pri disconnect;
    after-sb-1pri disconnect;
    after-sb-2pri disconnect;
    rr-conflict disconnect;
  }
  options {
    auto-promote no;
    quorum off;
  }
  handlers {
    split-brain "/run/current-system/sw/bin/expanse-drbd-event";
  }
  on n1 { node-id 0; address 192.168.1.1:7793; }
  on n2 { node-id 1; address 192.168.1.2:7793; }
  connection-mesh { hosts n1 n2; }
}
