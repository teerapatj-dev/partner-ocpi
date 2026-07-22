FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /bin/mock-ocpi-partner ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bin/mock-ocpi-partner /mock-ocpi-partner
EXPOSE 8080
ENTRYPOINT ["/mock-ocpi-partner"]
