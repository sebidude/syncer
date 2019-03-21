FROM alpine:latest as builder

RUN apk update && apk upgrade
RUN apk add --no-cache shadow ca-certificates
RUN useradd -u 10001 syncer
RUN mkdir /data

FROM scratch

COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder --chown=10001 /data /data
COPY build/linux/syncer /syncer

USER ruler
ENTRYPOINT ["/syncer"]