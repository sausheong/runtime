# Build context MUST be the parent directory containing BOTH `runtime/` and
# `harness/` (runtime's go.mod has: replace github.com/sausheong/harness => ../harness).
# Build from the projects root:
#   docker build -f runtime/deploy/Dockerfile -t runtime .
FROM golang:1.25 AS build
WORKDIR /src
COPY harness/ ./harness/
COPY runtime/ ./runtime/
WORKDIR /src/runtime
RUN go build -o /out/runtimed ./cmd/runtimed && go build -o /out/agentd ./cmd/agentd

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/runtimed /app/runtimed
COPY --from=build /out/agentd /app/agentd
COPY runtime/runtime.yaml /app/runtime.yaml
ENV RUNTIME_AGENTD_BIN=/app/agentd
EXPOSE 8080
CMD ["/app/runtimed"]
