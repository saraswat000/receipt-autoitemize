FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/server /app/server
COPY fixtures /app/fixtures
ENV ADDR=:8080 DATA_DIR=/tmp/data FIXTURES_DIR=/app/fixtures/task-a LOG_FORMAT=json
EXPOSE 8080
ENTRYPOINT ["/app/server"]
