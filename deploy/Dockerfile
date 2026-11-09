# tollbooth container image: one static binary on a distroless base, running as a non-root user.
#
#   docker build -f deploy/Dockerfile -t tollbooth:dev .
#   docker run --rm -p 8443:8443 -p 127.0.0.1:9900:9900 \
#     -v /srv/tollbooth/showback:/var/lib/tollbooth/showback \
#     -v /srv/tollbooth/tollbooth.yaml:/etc/tollbooth/tollbooth.yaml:ro \
#     tollbooth:dev
#
# The showback directory must be a persistent volume (docs/SHOWBACK_SCHEMA.md): a missing hour file
# is downtime. The admin listener binds 127.0.0.1 by default; publish it only to a management network.
# GOMEMLIMIT is the memory target the specification measures against (docs/OPERATIONS.md, Sizing).

# Build stage: the same Go the repository pins (go.mod `go` line), CGO off for a static binary.
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o /out/tollbooth ./cmd/tollbooth \
 && /out/tollbooth version

# Runtime stage: distroless static (CA certificates, /etc/passwd with nonroot, nothing else).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tollbooth /usr/local/bin/tollbooth
COPY deploy/tollbooth.example.yaml /etc/tollbooth/tollbooth.yaml
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/doc/tollbooth/
ENV GOMEMLIMIT=1GiB
USER nonroot:nonroot
VOLUME ["/var/lib/tollbooth/showback"]
EXPOSE 8443 9900
ENTRYPOINT ["/usr/local/bin/tollbooth"]
CMD ["serve", "--config", "/etc/tollbooth/tollbooth.yaml"]
