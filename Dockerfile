FROM node:20-alpine AS frontend-build
WORKDIR /src
COPY go.mod go.sum VERSION ./
COPY web/package.json web/package-lock.json ./
RUN npm ci --prefix web
COPY web/ web/
RUN npm run build --prefix web

FROM golang:1.24-alpine AS backend-build
WORKDIR /src
COPY go.mod go.sum VERSION ./
RUN go mod download
COPY --from=frontend-build /src/internal/admin/dist /src/internal/admin/dist
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /free-router .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend-build /free-router /free-router
ENV FREE_ROUTER_ADDR=0.0.0.0:1314
EXPOSE 1314
ENTRYPOINT ["/free-router"]
CMD ["serve"]
