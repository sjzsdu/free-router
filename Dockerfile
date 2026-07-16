FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum VERSION ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /free-router .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /free-router /free-router
EXPOSE 1314
ENTRYPOINT ["/free-router"]
