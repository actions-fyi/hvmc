# Image for the spike's static stub binaries (built by run.sh with
# CGO_ENABLED=0). No base image, so nothing is pulled.
FROM scratch
ARG BIN
COPY ${BIN} /stub
USER 65532:65532
ENTRYPOINT ["/stub"]
