FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mesh-relay ./cmd/mesh-relay

FROM scratch
COPY --from=build /out/mesh-relay /mesh-relay
USER 65532:65532
EXPOSE 8080 8443
ENTRYPOINT ["/mesh-relay"]
