FROM gcr.io/distroless/static:nonroot
COPY bin/anchorvm-controller /anchorvm-controller
USER 65532:65532
ENTRYPOINT ["/anchorvm-controller"]
