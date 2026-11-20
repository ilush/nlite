# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd/nlite/ ./cmd/nlite/
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w" -o /out/nlite ./cmd/nlite

FROM alpine:3.23
RUN addgroup -S -g 65532 nlite \
    && adduser -S -D -u 65532 -G nlite -h /home/nlite -s /bin/sh nlite \
    && chmod 0700 /home/nlite
COPY --from=build /out/nlite /nlite
ENV HOME=/home/nlite
USER 65532:65532
ENTRYPOINT ["/nlite"]
CMD ["serve", "--config", "/config/scenes.json"]
