FROM golang:1.27.0 AS build

# Build the existing test release; upstream no longer serves its container image.
RUN CGO_ENABLED=0 go install github.com/minio/minio@RELEASE.2025-04-22T22-12-26Z

FROM gcr.io/distroless/static-debian12
COPY --from=build /go/bin/minio /minio
ENTRYPOINT ["/minio"]
