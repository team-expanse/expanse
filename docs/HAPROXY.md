# HAProxy

How to deploy a `net/haproxy` block in front of services inside or outside the cluster, and what
clients see when a backend or a node fails.

## 1. The design, in one paragraph

`net/haproxy` runs [HAProxy](https://www.haproxy.org/) as a TCP and HTTP load balancer. HAProxy
keeps no state, so the block runs several replicas on different nodes behind one VIP, the way
`web/whoami` does; it needs no volume. Each replica health-checks every server itself and stops
sending to one that fails. The block writes `haproxy.cfg` from `config.frontends` on every start;
a whole `config` can be given instead. Expanse's own VIP load balancer already spreads traffic
over a block's replicas. Use HAProxy when you need what it adds: servers that are not blocks,
active health checks, balancing algorithms, and HAProxy's stats and metrics.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: lb
  namespace: default
spec:
  type: net/haproxy
  replicas: 2
  placement:
    antiAffinity: ANTI_AFFINITY_NODE
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
  config:
    statsPort: 18404
    frontends:
      - name: web
        port: 18080
        servers: ["app1.lan:8080", "app2.lan:8080"]
        checkPath: /healthz
      - name: db
        port: 15432
        mode: tcp
        balance: leastconn
        servers: ["10.0.0.31:5432", "10.0.0.32:5432"]
  network:
    ports:
      - name: http
        port: 80
        target_port: 18080
        protocol: tcp
        expose: EXPOSE_VIP
      - name: db
        port: 5432
        target_port: 15432
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 18080
        period_seconds: 2
```

Each frontend listens on its own `port` on every replica's node. Expose it on the VIP with a
`network.ports` entry whose `target_port` is that `port`; the exposed port must differ from it,
because the VIP holder binds the exposed port on a node that may run a replica. HAProxy runs
unprivileged, so frontend ports must be 1024 or above. The VIP spreads connections over the
replicas on every node, so a node firewall must let the other nodes reach each frontend `port`.

| Key | Default | Meaning |
|---|---|---|
| `frontends` | required, or `config` | Listeners, each balancing over its own servers |
| `frontends[].name` | required | Name in stats and metrics (`fe_<name>`, `be_<name>`) |
| `frontends[].port` | required | Listen port |
| `frontends[].servers` | required | Servers as `host:port`; names resolve at start |
| `frontends[].mode` | `http` | `http` adds `X-Forwarded-For` and checks with an HTTP GET; `tcp` forwards bytes |
| `frontends[].balance` | `roundrobin` | `roundrobin`, `leastconn`, `source` or `first` |
| `frontends[].check` | `true` | Check servers every 2 s; two failures take one out, two passes bring it back |
| `frontends[].checkPath` | `/` | HTTP mode: the path checked; any 2xx or 3xx passes |
| `statsPort` | none | Serves the stats page at `/stats` and Prometheus metrics at `/metrics` |
| `maxconn` | `4096` | Global connection limit |
| `config` | none | A whole `haproxy.cfg`, used instead of everything above |

A server name that does not resolve when HAProxy starts leaves that server down instead of
stopping HAProxy. A name takes the first address the node's resolver returns, which is IPv6 when the
name has one; give an IPv4 address instead if the server listens on IPv4 only. The block does not
resolve names again later; a redeploy does. The stats port
is not on the VIP: read each replica at its node's address, or add it to `network.ports` without
`expose`. Each node must have `pkgs.haproxy` in `environment.systemPackages`.

## 3. Failures, from a client's perspective

**A server fails.** Each replica takes it out of rotation after two failed checks, 2 to 4 s later.
Requests already sent to it fail. When it passes two checks it gets traffic again.

**A replica's node fails.** If it held the VIP, the VIP moves to a node that runs the other
replica, and connections through it are lost; clients reconnect to the same address. The
controller then places a new replica on a spare node, if there is one.

Measured in the `net-haproxy` VM test (3 nodes, 5 s agent period): a stopped server left rotation
within the check window with no failed requests, and after the node holding the VIP was killed,
requests through the VIP succeeded again 6 to 15 s later. Both replicas were `RUNNING` again,
one of them on the spare node, 49 s after the crash.

## 4. What is not covered

- TLS termination in the generated config; use `config` with `bind ... ssl crt`, or put
  `web/caddy` in front.
- Re-resolving server names while running, and DNS service discovery.
- Sticky sessions beyond `balance: source`.
- UDP: VIPs forward TCP only.
