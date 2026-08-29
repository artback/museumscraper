# One image, one binary, every subcommand. The services and the batch jobs
# differ only in the arguments they are started with, so there is nothing to
# gain from separate images and a great deal of duplication to avoid.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied first so the module download layer is reused whenever
# only application code has changed.
#
# The nested module's manifests come too, and must: the root go.mod replaces
# github.com/artback/museumscraper/extract with ./extract, so "go mod download"
# reads extract/go.mod before it reads anything from the network. Without them
# it fails in about a second with "reading extract/go.mod: no such file or
# directory" — which is what broke every deploy from the day the extract module
# landed, while the tests stayed green because a checkout has the directory and
# a build context of two files does not.
COPY go.mod go.sum ./
COPY extract/go.mod extract/go.sum ./extract/
RUN go mod download

COPY . .

# CGO is not needed and disabling it produces a static binary the runtime stage
# can use without a libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/museum ./cmd/museum

FROM alpine:3.20

# Certificates are required: every source — Wikidata, Wikipedia, Overpass,
# Nominatim, and the museum websites themselves — is HTTPS. tzdata keeps
# exhibition dates honest across time zones.
RUN apk add --no-cache ca-certificates tzdata

# The crawl reaches the public internet, so it runs unprivileged.
RUN adduser -D -u 10001 museum
USER museum

COPY --from=build /out/museum /usr/local/bin/museum

EXPOSE 8090

# No default subcommand: starting the wrong one is worse than starting none,
# and every deployment names the one it wants.
ENTRYPOINT ["museum"]
CMD ["help"]
