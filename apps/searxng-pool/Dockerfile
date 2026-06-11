# Build a tiny static binary, then ship it on scratch.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /searxng-pool .

FROM gcr.io/distroless/static-debian12
COPY --from=build /searxng-pool /searxng-pool
EXPOSE 8889
ENTRYPOINT ["/searxng-pool"]
