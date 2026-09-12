# Da Vinci PAS payer server (Go). Build from the go/ directory; the adjudication rules are bundled in the binary:
#   docker build -t nirmitee/davinci-pas-server-go go/
#   docker run --rm -p 8085:8085 nirmitee/davinci-pas-server-go
# Mount a different rules file with -v and PAS_RULES_FILE. The test-only control routes stay disabled unless
# PAS_ADMIN_ENABLED=true is passed for a conformance run; never set it in this image.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pas-server ./cmd/pas-server \
 && mkdir -p /out/data/audit

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/pas-server /app/pas-server
COPY --from=build --chown=65532:65532 /out/data /data
ENV PAS_PORT=8085 \
    PAS_DATA_DIR=/data
EXPOSE 8085
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/app/pas-server"]
