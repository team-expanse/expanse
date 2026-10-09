# Plugins the monitor/grafana block links into Grafana: 13.2 split its Prometheus
# datasource out of the core. Ship this beside pkgs.grafana; NixOS links /lib into the profile.
{ linkFarm, grafanaPlugins }:
linkFarm "expanse-grafana-plugins" [
  { name = "lib/grafana/plugins/prometheus"; path = grafanaPlugins.prometheus; }
]
