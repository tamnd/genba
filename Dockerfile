# GoReleaser builds the binary and this image only copies it in, so the image is
# the binary, its CA certificates and nothing else. There is no shell in here to
# get a foothold in and nothing to keep patched.
FROM gcr.io/distroless/static:nonroot

COPY genbad /usr/bin/genbad

EXPOSE 8080
USER nonroot:nonroot

ENV GENBA_ADDR=0.0.0.0:8080

ENTRYPOINT ["/usr/bin/genbad"]
