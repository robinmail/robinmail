# robinmail with the webmail inside, as "make" builds it.
#
#   docker build -t robinmail .
#
# The image holds the binary and nothing else of ours: the config, the
# master key and the certificate are mounted, the data directory is a
# volume. contrib/compose/cluster.yaml runs three of them.

FROM node:26-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/web/dist/ internal/web/dist/
RUN CGO_ENABLED=0 go build -tags webmail -trimpath -o /robinmail ./cmd/robinmail

FROM alpine:3.22
COPY --from=build /robinmail /usr/local/bin/robinmail
VOLUME /var/lib/robinmail
ENTRYPOINT ["robinmail"]
CMD ["-config", "/etc/robinmail/robinmail.json", "serve"]
