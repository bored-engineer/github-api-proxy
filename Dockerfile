# syntax=docker/dockerfile:1

FROM cgr.dev/chainguard/static:latest
ARG TARGETARCH
COPY dist/github-api-proxy_linux_${TARGETARCH} /usr/bin/github-api-proxy
EXPOSE 44879
ENTRYPOINT ["/usr/bin/github-api-proxy"]
