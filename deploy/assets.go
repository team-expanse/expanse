// Package deploy embeds the shipped Prometheus rules and Grafana dashboards
// so the monitor/* blocks run exactly the tested files.
package deploy

import _ "embed"

//go:embed prometheus/expanse-alerts.rules.yml
var AlertRules []byte

//go:embed grafana/dashboards/expanse-cluster-health.json
var ClusterHealthDashboard []byte
