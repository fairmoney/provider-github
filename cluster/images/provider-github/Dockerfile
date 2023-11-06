FROM gcr.io/distroless/static@sha256:14520cd6b9317b48eb8343280370bd97f7eab43742026668cbcb28e2906696e7
LABEL org.opencontainers.image.source=https://github.com/fairmoney/provider-github
ARG TARGETOS
ARG TARGETARCH

ADD bin/$TARGETOS\_$TARGETARCH/provider /usr/local/bin/crossplane-github-provider

USER 65532
ENTRYPOINT ["crossplane-github-provider"]
