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

Setting `service.upstreams` will override the default list of upstreams. It's good idea to forward DNS requests to the Pi-hole/AdGuard DNS.

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

Exposing DNS server to the LAN clients could be tricky, NodePort can't bind below 30000. So either `HostNetwork=true`, out-of-cluster deployment, or host level proxy.
I personally opted in with host level proxy: Nginx. Nginx is a lightweight reverse proxy which can be configured to load balance `:53/udp` port. The Nginx is included in almost all distros, so it shouldn't be a problem to install Nginx on the host system.

Below is my minimalistic nginx configuration file:
```bash
$ cat /etc/nginx/nginx.conf

worker_processes  4;
events {
  worker_connections  4096;
}
stream {
  upstream dns_server {
    server 127.0.0.1:30053;
  }
  server {
    listen 53 udp;
    proxy_pass dns_server;
  }
}
```

## Development

* Setup `pre-commit` [docs](https://pre-commit.com/#installation)
* Have fun!
