# Static binary, distroless runtime.
#
# AC-1 (<10s cold start) and AC-3 (<150MB) both fall out of this. The Hermes
# harness failed both -- 180s to boot and a per-user image carrying Chromium and
# 71 bundled skills under s6 -- and neither cost is inherent to a harness.
#
# No supervisor and no docker --init: one process, PID 1, signals handled in
# main. Hermes needed s6 and that is precisely why it could not be given a
# container User.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# The tests ARE this image's acceptance check, so they run in the build and a
# failure stops the publish -- the same discipline deploy/picoclaw-glob applies
# to its patches.
RUN go vet ./... && go test ./...
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/crab-ganglion ./cmd/crab-ganglion

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/crab-ganglion /usr/local/bin/crab-ganglion
# 1000:1000 matches what crab-shell-proxy already chowns per-user volumes to.
USER 1000:1000
EXPOSE 18800
ENTRYPOINT ["/usr/local/bin/crab-ganglion"]
