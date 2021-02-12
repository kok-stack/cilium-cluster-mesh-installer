# Build the manager binary
FROM golang:1.13 as builder

RUN go env -w GOPROXY=https://goproxy.cn,direct

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GO111MODULE=on GOPROXY=https://goproxy.cn,direct go build -a -o main main.go

FROM ccr.ccs.tencentyun.com/k8sonk8s/proxy:helm-arm64 as helm-arm64
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM ubuntu
WORKDIR /
COPY --from=helm-arm64 /linux-arm64/helm /usr/local/bin/helm
COPY --from=builder /workspace/main .
#helm , cilium , chmod 777
RUN apt update && apt install -y curl wget jq
ADD extract-etcd-secrets.sh /
ADD generate-name-mapping.sh /
ADD generate-secret-yaml.sh /
RUN chmod 777 /*.sh

CMD ["./main"]
