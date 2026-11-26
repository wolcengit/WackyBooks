FROM golang:1.25-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY app/ ./app/
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/wackybooks ./app

FROM scratch AS binary
COPY --from=build /out/wackybooks /wackybooks

FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates-bundle poppler-utils \
    && mkdir -m 0700 /data \
    && chown 10001:10001 /data

COPY --from=binary /wackybooks /usr/local/bin/wackybooks

ENV WACKY_ADDR=0.0.0.0:8080 \
    WACKY_DATA=/data
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/wackybooks"]
