# Butler Bootstrap Controller
FROM golang:1.24-alpine AS builder
WORKDIR /workspace
RUN apk add --no-cache git make
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o manager cmd/main.go

# Runtime with tools
FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl bash openssh-client openssl

# Install talosctl
ARG TALOS_VERSION=v1.9.0
RUN curl -Lo /usr/local/bin/talosctl \
    "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-amd64" && \
    chmod +x /usr/local/bin/talosctl

# Install kubectl
ARG KUBECTL_VERSION=v1.31.2
RUN curl -Lo /usr/local/bin/kubectl \
    "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" && \
    chmod +x /usr/local/bin/kubectl

# Install clusterctl
RUN curl -L https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.9.4/clusterctl-linux-amd64 -o /usr/local/bin/clusterctl && \
    chmod +x /usr/local/bin/clusterctl

# Install helm
RUN curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# Create nonroot user and home directory
RUN adduser -D -u 65532 -h /home/nonroot nonroot

# Configure clusterctl with community providers
RUN mkdir -p /home/nonroot/.cluster-api && \
    chown -R 65532:65532 /home/nonroot

COPY --chown=65532:65532 <<EOF /home/nonroot/.cluster-api/clusterctl.yaml
providers:
  - name: "harvester"
    url: "https://github.com/rancher-sandbox/cluster-api-provider-harvester/releases/latest/infrastructure-components.yaml"
    type: "InfrastructureProvider"
  - name: "nutanix"
    url: "https://github.com/nutanix-cloud-native/cluster-api-provider-nutanix/releases/latest/infrastructure-components.yaml"
    type: "InfrastructureProvider"
  - name: "kamaji"
    url: "https://github.com/clastix/cluster-api-control-plane-provider-kamaji/releases/latest/control-plane-components.yaml"
    type: "ControlPlaneProvider"
EOF

COPY --from=builder /workspace/manager /manager

USER 65532:65532
ENV HOME=/home/nonroot
ENTRYPOINT ["/manager"]
