FROM gcr.io/distroless/static@sha256:41972110a1c1a5c0b6adb283e8aa092c43c31f7c5d79b8656fbffff2c3e61f05
LABEL org.opencontainers.image.source=https://github.com/fairmoney/provider-github

ARG TARGETOS
ARG TARGETARCH

ADD bin/$TARGETOS\_$TARGETARCH/provider /usr/local/bin/crossplane-github-provider

USER 65532
ENTRYPOINT ["crossplane-github-provider"]
