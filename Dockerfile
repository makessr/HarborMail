# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.23 AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG HARBOR_GO_PROXY=https://proxy.golang.org,direct
ARG HARBOR_GO_SUMDB=sum.golang.org
ARG HARBOR_BUILD_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY="$HARBOR_GO_PROXY" GOSUMDB="$HARBOR_GO_SUMDB" go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/ web/
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    GOPROXY="$HARBOR_GO_PROXY" GOSUMDB="$HARBOR_GO_SUMDB" \
    go build -trimpath -ldflags="-s -w -X harbormail/internal/server.buildVersion=${HARBOR_BUILD_VERSION} -X harbormail/internal/server.buildDeployment=docker" -o /out/harbormail ./cmd/harbormail

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 harbor \
    && adduser -S -D -H -u 10001 -G harbor harbor \
    && mkdir -p /var/lib/harbormail \
    && chown harbor:harbor /var/lib/harbormail
COPY --from=build /out/harbormail /usr/local/bin/harbormail
COPY docs/licenses/go-qrcode.txt /usr/share/licenses/harbormail/go-qrcode.txt
COPY docs/licenses/go-msgauth.txt /usr/share/licenses/harbormail/go-msgauth.txt
COPY docs/licenses/webpush-go.txt docs/licenses/jwt-v5.txt /usr/share/licenses/harbormail/
USER 10001:10001
WORKDIR /var/lib/harbormail
ENV HARBOR_WEB=0.0.0.0:8080 HARBOR_SMTP=0.0.0.0:2525 HARBOR_DB=/var/lib/harbormail/harbor.db \
    HARBOR_DKIM_LOCAL_KEY_FILE=/var/lib/harbormail/dkim-key
EXPOSE 8080 2525 2465 2587 2993 2995
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/harbormail", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/harbormail"]
