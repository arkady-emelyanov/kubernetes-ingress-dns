# Kubernetes Ingress DNS

A dead-simple DNS server for home labs.

<i>Makes ingresses just work on your LAN!</i>

This DNS server only responds to known Ingress hosts. All other domain queries are forwarded to your configured upstream DNS servers.

For best results, it's a good idea to point the upstream to a Pi-hole or AdGuard installation.


## Prerequisites

* K3S
* MetalLB
* Any Ingress Controller (Traefik, Nginx, Istio)

## Installation

Using helm
```bash
helm repo add kubernetes-ingress-dns https://arkady-emelyanov.github.io/kubernetes-ingress-dns/
helm repo update
helm install --set service.upstreams="4.4.4.4:53,1.1.1.1:53" ingress-dns kubernetes-ingress-dns/kubernetes-ingress-dns
```

Setting `service.upstreams` will override the default list of upstreams.

DNS Server supports following upstream types:
* `tcp://host:port` - for tcp-based upstreams
* `upd://host:port` - for udp-based upstreams
* `host:port` - default, maps to udp

## Example service

Install httpbin:
```bash
helm repo add matheusfm https://matheusfm.dev/charts
helm repo update
helm install --set ingress.enabled=true httpbin matheusfm/httpbin
```

Query the DNS server:
```bash
nslookup -port=30053 httpbin.local <k3s node ip>
```

## Using as a LAN-level DNS

Exposing DNS server to the LAN clients could be tricky: NodePort can't bind below `30000`. 

Deployment ideas: `HostNetwork=true`, out-of-cluster, or host level proxy.

I personally opted in with out-of-cluster deployment.
Just make sure root user is able to access the Kubernetes cluster.

Sample systemd unit:
```
[Unit]
Description=Kubernetes Ingress DNS
After=network.target
Wants=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/kubernetes-ingress-dns -upstreams "8.8.8.8:53,4.4.4.4:53" -dns-port=53 -http-port=8053
Restart=on-failure
User=root

[Install]
WantedBy=multi-user.target
```


## Development

* Setup `pre-commit` [docs](https://pre-commit.com/#installation)
* Have fun!
