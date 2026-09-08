# --- frontend ---------------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY web/ .
RUN npm run build

# --- backend ----------------------------------------------------------------
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /cloudroof ./cmd/cloudroof

# --- runtime ----------------------------------------------------------------
# scratch would work (static binary) but we want CA certs for provider APIs
# and a shell for `docker exec` debugging.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -h /data -u 1000 cloudroof
COPY --from=build /cloudroof /usr/local/bin/cloudroof
USER cloudroof
VOLUME /data
EXPOSE 7070
ENV CLOUDROOF_ADDR=:7070 CLOUDROOF_DATA=/data
ENTRYPOINT ["cloudroof"]
