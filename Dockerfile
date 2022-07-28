FROM quay.io/bitnami/golang:1.17 AS builder
WORKDIR /go/src/stolostron/hub-of-hubs-addon-controller
COPY . .
ENV GO_PACKAGE stolostron/hub-of-hubs-addon-controller

RUN make build --warn-undefined-variables

FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
ENV USER_UID=10001

COPY --from=builder /go/src/stolostron/hub-of-hubs-addon-controller/bin/ /
COPY --from=builder /go/src/stolostron/hub-of-hubs-addon-controller/pkg/agent/manifests /manifests
RUN microdnf update && microdnf clean all

USER ${USER_UID}
