FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /ledger ./cmd/ledger
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ledger /ledger
EXPOSE 8080
ENTRYPOINT ["/ledger"]
