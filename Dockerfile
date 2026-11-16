# Compile in a Go image, then copy only the executable into the runtime image.
FROM golang:1.25-bookworm AS build
WORKDIR /src
# Copy dependency manifests first so source-only edits can reuse this cache layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Disable C linking for the minimal runtime; trim local build paths from output.
RUN CGO_ENABLED=0 go build -trimpath -o /ledger ./cmd/ledger
# The runtime needs no Go compiler or shell and runs under a non-root account.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ledger /ledger
# EXPOSE documents the port; Compose's ports setting publishes it on the host.
EXPOSE 8080
ENTRYPOINT ["/ledger"]
