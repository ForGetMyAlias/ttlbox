FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ttlbox ./cmd/ttlbox \
 && mkdir /data

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/ttlbox /ttlbox
COPY --from=build --chown=65532:65532 /data /data
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/ttlbox"]
