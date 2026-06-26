# Build stage — used when building without Nix (e.g. contributors without Nix)
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -extldflags=-static" -o /smeltry-operator .

# Final image — also used when the binary is built externally via `nix build`
FROM scratch
COPY --from=build /smeltry-operator /smeltry-operator
ENTRYPOINT ["/smeltry-operator"]
