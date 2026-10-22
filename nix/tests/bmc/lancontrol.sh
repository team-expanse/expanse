#!/bin/sh
# ipmi_sim's lan_config_program: a fixed iDRAC-like LAN config for the simulated BMC.
[ "$2" = get ] || exit 0
printf '%s\n' ip_addr:10.0.40.17 ip_addr_src:static mac_addr:d0:94:66:1a:2b:3c \
  subnet_mask:255.255.255.0 default_gw_ip_addr:10.0.40.1
