FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /bin/mock-ocpi-partner ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -o /bin/ocpi-demo ./cmd/demo

FROM gcr.io/distroless/static-debian12:nonroot AS demo
COPY --from=build /bin/ocpi-demo /ocpi-demo
EXPOSE 8080
ENTRYPOINT ["/ocpi-demo"]

# Last stage stays the default target so a plain `docker build .` (and the base
# compose file) still produces the partner server image.
FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /bin/mock-ocpi-partner /mock-ocpi-partner
EXPOSE 8080
ENTRYPOINT ["/mock-ocpi-partner"]
