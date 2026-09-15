FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S clickdog && adduser -S clickdog -G clickdog

# Bare declaration on purpose: buildx auto-injects linux/amd64 /
# linux/arm64 here, and a Dockerfile-level default would shadow it
# (which is what broke v26.05.1-alpha's first publish attempt).
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/click-dog /usr/local/bin/click-dog

USER clickdog

ENTRYPOINT ["click-dog"]
CMD ["-config", "/etc/click-dog/click-dog.yaml"]
