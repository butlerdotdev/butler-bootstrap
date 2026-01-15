# syntax=docker/dockerfile:1.6

########################################
# Builder
########################################
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

WORKDIR /workspace

RUN apk add --no-cache git make

# BuildKit-provided args
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /workspace/manager \
      cmd/main.go

########################################
# Runtime
########################################
FROM alpine:3.19

# BuildKit-provided args
ARG TARGETARCH

RUN apk add --no-cache \
      ca-certificates \
      curl \
      bash \
      openssh-client \
      openssl

########################################
# Install talosctl
########################################
ARG TALOS_VERSION=v1.9.0
RUN curl -fsSL \
      "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-${TARGETARCH}" \
      -o /usr/local/bin/talosctl && \
    chmod +x /usr/local/bin/talosctl

########################################
# Install kubectl
########################################
ARG KUBECTL_VERSION=v1.31.2
RUN curl -fsSL \
      "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
      -o /usr/local/bin/kubectl && \
    chmod +x /usr/local/bin/kubectl

########################################
# Install clusterctl
########################################
ARG CLUSTERCTL_VERSION=v1.9.4
RUN curl -fsSL \
      "https://github.com/kubernetes-sigs/cluster-api/releases/download/${CLUSTERCTL_VERSION}/clusterctl-linux-${TARGETARCH}" \
      -o /usr/local/bin/clusterctl && \
    chmod +x /usr/local/bin/clusterctl

########################################
# Install helm (script auto-detects arch)
########################################
RUN curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

########################################
# Non-root user
########################################
RUN adduser -D -u 65532 -h /home/nonroot nonroot && \
    mkdir -p /home/nonroot/.cluster-api && \
    chown -R 65532:65532 /home/nonroot

########################################
# clusterctl config
########################################
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

########################################
# Copy manager binary
########################################
COPY --from=builder /workspace/manager /manager

########################################
# Runtime config
########################################
USER 65532:65532
ENV HOME=/home/nonroot

ENTRYPOINT ["/manager"]
