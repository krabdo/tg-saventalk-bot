FROM golang:1.26-alpine AS build
RUN apk add --no-cache gcc musl-dev ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=1 go build -trimpath -tags "netgo osusergo sqlite_omit_load_extension" -ldflags '-s -w -linkmode external -extldflags "-static"' -o /bot .  && mkdir -p /out/data && chown 1000:1000 /out/data

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/krabdo/tg-saventalk-bot"
LABEL org.opencontainers.image.licenses="GPL-3.0"
COPY --from=build /bot /bot
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=1000:1000 /out/data /data
COPY LICENSE /LICENSE
ENV DATA_DIR=/data GOMEMLIMIT=32MiB
USER 1000:1000
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s CMD ["/bot", "healthcheck"]
ENTRYPOINT ["/bot"]
