# Caddy

How to deploy a `web/caddy` block, put sites behind it, and what clients see when the node serving
it fails.

## 1. The design, in one paragraph

`web/caddy` runs [Caddy](https://caddyserver.com/) as a web server and reverse proxy with automatic
HTTPS. It is active/passive like `storage/s3`: the block is `SINGLETON`, it runs only where its
volume's DRBD primary is, and clients reach it through a stable VIP that exposes both 80 and 443.
Caddy's data directory lives on the volume. It holds the certificates, their keys, the ACME account
and Caddy's internal CA. The node that takes over after a failover serves the same certificates
instead of requesting new ones, which matters with Let's Encrypt's rate limits. The block writes
the Caddyfile from `config.sites` on every start; a whole `caddyfile` can be given instead.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: edge
  namespace: default
spec:
  type: web/caddy
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: caddy-data
      size: 1Gi
      replication: 3
      mountPath: /var/lib/caddy
  config:
    email: ops@example.com
    sites:
      - host: app.example.com
        reverseProxy: ["10.0.0.21:8080", "10.0.0.22:8080"]
      - host: status.example.com
        tls: "off"
        respond: ok
  network:
    ports:
      - name: http
        port: 80
        target_port: 18080
        protocol: tcp
        expose: EXPOSE_VIP
      - name: https
        port: 443
        target_port: 18443
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 18080
        period_seconds: 2
```

Point each site's DNS name at the block's VIP (`expanse ctl block get edge`). `port` and
`httpsPort` must differ from the exposed 80 and 443, because the VIP holder binds those on the node
Caddy runs on. Caddy runs unprivileged, so it could not bind them anyway.

| Key | Default | Meaning |
|---|---|---|
| `sites` | required, or `caddyfile` | Sites served, one host name each |
| `sites[].host` | required | Host name; `*.example.com` needs a DNS challenge, so use `caddyfile` for it |
| `sites[].tls` | `acme` | `acme`: a public certificate; `internal`: Caddy's own CA; `off`: plain HTTP only |
| `sites[].reverseProxy` | one of these two | Upstreams as `host:port`, round robin; a failing upstream is skipped for 30 s |
| `sites[].respond` | one of these two | A fixed one-line body, for a placeholder or a health endpoint |
| `email` | none | ACME account email |
| `acmeCA` | Let's Encrypt, then ZeroSSL | ACME directory URL, such as Let's Encrypt's staging server |
| `port` | `18080` | Caddy's HTTP port (`target_port` of 80) |
| `httpsPort` | `18443` | Caddy's HTTPS port (`target_port` of 443) |
| `caddyfile` | none | A whole Caddyfile, used instead of everything above |

A site with `tls: acme` or `tls: internal` answers HTTPS, and its plain-HTTP requests get a 308
redirect to `https://<host>`. The block writes these redirects itself: Caddy's own would point at
`httpsPort` instead of 443. ACME's HTTP challenge is answered on port 80, so the VIP must be
reachable from the internet on 80 or 443 for `acme` sites. Use `tls: internal` on a private
network; clients then need Caddy's root certificate, which is at
`caddy/pki/authorities/local/root.crt` on the volume.

With `caddyfile`, the block writes nothing for you: set the listening ports with the `http_port`
and `https_port` global options, and the redirects too if you need them. The data directory is
still the volume. Each node must have `pkgs.caddy` in `environment.systemPackages`.

## 3. Failover, from a client's perspective

When the node serving the block dies, the block, its volume's DRBD primary and its VIP move
together to a survivor, and Caddy starts there with the same data directory. Requests in flight
fail and clients reconnect to the same address. The survivor serves the same certificate. A
certificate Caddy was renewing at the moment of the crash is renewed again on the survivor.

Caddy never syncs the files it writes, so the block flushes the volume every 2 s while Caddy runs.
A certificate issued in the last 2 s before a crash can be lost; the survivor then issues it again.
Without the flush the window was ext4's 30 s, and the first crash test lost a fresh certificate.

Measured in the `web-caddy` VM test (3 nodes, 5 s agent period): the block, its volume primary and
its VIP agreed on a survivor 55 s after the serving VM was killed, and HTTPS answered again a
fraction of a second later with the same certificate and the same internal CA.

## 4. What is not covered

- More than one Caddy serving the same sites. Caddy can share certificates through a storage
  backend all instances reach, but the shipped block keeps them on its own volume.
- Caddy's admin API, which is off; a config change is a redeploy, which restarts Caddy.
- Serving files: use `web/static-site` or `web/nginx`, or a `caddyfile` with `file_server`.
- HTTP/3 over UDP: VIPs forward TCP only.
