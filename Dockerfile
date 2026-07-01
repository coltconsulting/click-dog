FROM alpine:3.20.10@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

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
