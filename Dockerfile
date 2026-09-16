FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY . .
RUN test -f qday/core/go.mod && \
    CGO_ENABLED=1 go test ./... && \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/qday-walletd .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/qday-walletd /usr/local/bin/qday-walletd
VOLUME ["/var/lib/qday-walletd"]
EXPOSE 19771/tcp 19772/tcp
ENTRYPOINT ["qday-walletd"]
CMD ["run", "-data", "/var/lib/qday-walletd", "-listen", "0.0.0.0:19772", "-allow-remote-api"]
