# Self-host ascii-tv anywhere that runs containers.
#   docker build -t ascii-tv .
#   docker run -p 8080:8080 ascii-tv
# Add your own movies by mounting a catalog:
#   docker run -p 8080:8080 -v $PWD/movies:/movies ascii-tv \
#     -catalog /app/resources/catalog.json -catalog /movies/catalog.private.json
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ascii-tv .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /ascii-tv /app/ascii-tv
COPY resources /app/resources
EXPOSE 8080
ENTRYPOINT ["/app/ascii-tv"]
