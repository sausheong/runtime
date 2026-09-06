# Build context is the runtime repository root:
#   docker build -f deploy/Dockerfile -t runtime:latest .
FROM golang:1.27.1@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS build
WORKDIR /src/runtime
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/runtimed   ./cmd/runtimed \
 && CGO_ENABLED=0 go build -trimpath -o /out/agentd     ./cmd/agentd \
 && CGO_ENABLED=0 go build -trimpath -o /out/sandboxd   ./cmd/sandboxd \
 && CGO_ENABLED=0 go build -trimpath -o /out/browserd   ./cmd/browserd \
 && CGO_ENABLED=0 go build -trimpath -o /out/runtimectl ./cmd/runtimectl \
 && CGO_ENABLED=0 go build -trimpath -o /out/v1-probe   ./cmd/v1-probe

FROM scratch
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="runtime" \
      org.opencontainers.image.source="https://github.com/sausheong/runtime" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/runtimed   /app/runtimed
COPY --from=build /out/agentd     /app/agentd
COPY --from=build /out/sandboxd   /app/sandboxd
COPY --from=build /out/browserd   /app/browserd
COPY --from=build /out/runtimectl /app/runtimectl
COPY --from=build /out/v1-probe   /app/v1-probe
COPY runtime.yaml /app/runtime.yaml
ENV RUNTIME_AGENTD_BIN=/app/agentd \
    PATH=/app:$PATH
EXPOSE 8080
USER 10001
CMD ["/app/runtimed"]
