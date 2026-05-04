FROM gcr.io/distroless/static:nonroot

ARG TARGETOS
ARG TARGETARCH

COPY ${TARGETOS}/${TARGETARCH}/grosz /usr/bin/grosz

EXPOSE 8887 3000

USER nonroot:nonroot

ENTRYPOINT ["/usr/bin/grosz"]
