FROM golang:1.23 AS builder

RUN apt-get update && apt-get install -y ca-certificates && update-ca-certificates

WORKDIR /app
COPY go.mod go.sum /app

RUN go mod download
COPY . /app

RUN CGO_ENABLED=0 GOOS=linux go build -o kubernetes-ingress-dns .

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/kubernetes-ingress-dns /kubernetes-ingress-dns

EXPOSE 53
ENTRYPOINT ["/kubernetes-ingress-dns"]
