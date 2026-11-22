# syntax=docker/dockerfile:1

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/k8s-sentinel ./cmd/server

# distroless static:nonroot has no shell, no package manager, and already
# runs as a real non-root numeric UID (65532) declared in the image itself
# — exactly the property Phase 4's README "Known limitations" entry found
# missing in nicolaka/netshoot, so this tool's own image doesn't hit that
# same runAsNonRoot failure mode.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/k8s-sentinel /k8s-sentinel
USER 65532:65532
ENTRYPOINT ["/k8s-sentinel"]
