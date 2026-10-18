FROM node:20-alpine AS frontend-build
WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN npm ci --prefix web
COPY web/ web/
RUN npm run build --prefix web

FROM golang:1.24-alpine AS backend-build
WORKDIR /src
COPY go.mod go.sum VERSION ./
RUN go mod download
COPY . .
COPY --from=frontend-build /src/internal/admin/dist ./internal/admin/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/free-router .

FROM alpine:3.22 AS runtime-base
RUN addgroup -S -g 65532 free-router \
    && adduser -S -D -H -u 65532 -G free-router free-router \
    && mkdir -p /data \
    && chown free-router:free-router /data

FROM runtime-base
COPY --from=backend-build /out/free-router /usr/local/bin/free-router
ENV FREE_ROUTER_ADDR=0.0.0.0:1314 \
    FREE_ROUTER_CACHE=/data/models.json \
    FREE_ROUTER_CONFIG=/data/config.json \
    FREE_ROUTER_CREDENTIALS=/data/credentials.json \
    FREE_ROUTER_HEALTH=/data/health.json \
    FREE_ROUTER_STATISTICS=/data/statistics.json
EXPOSE 1314
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget --quiet --tries=1 --output-document=- http://127.0.0.1:1314/healthz >/dev/null || exit 1
USER free-router:free-router
ENTRYPOINT ["/usr/local/bin/free-router"]
CMD ["serve"]
