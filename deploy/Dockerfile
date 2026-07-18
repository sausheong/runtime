# Build context is the runtime repository root:
#   docker build -f deploy/Dockerfile -t runtime:latest .
FROM golang:1.25 AS build
WORKDIR /src/runtime
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN go build -o /out/runtimed   ./cmd/runtimed \
 && go build -o /out/agentd     ./cmd/agentd \
 && go build -o /out/sandboxd   ./cmd/sandboxd \
 && go build -o /out/browserd   ./cmd/browserd \
 && go build -o /out/runtimectl ./cmd/runtimectl \
 && go build -o /out/v1-probe   ./cmd/v1-probe

FROM debian:bookworm-slim
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="runtime" \
      org.opencontainers.image.source="https://github.com/sausheong/runtime" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
RUN apt-get update && apt-get install -y ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin runtime
WORKDIR /app
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
