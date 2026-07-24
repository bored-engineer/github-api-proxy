# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go:latest AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/github-api-proxy .

FROM cgr.dev/chainguard/static:latest
COPY --from=build /out/github-api-proxy /usr/bin/github-api-proxy
EXPOSE 44879
ENTRYPOINT ["/usr/bin/github-api-proxy"]
