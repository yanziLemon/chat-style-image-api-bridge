FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/chat-style-image-api-bridge ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/chat-style-image-api-bridge /chat-style-image-api-bridge
ENV LISTEN_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/chat-style-image-api-bridge"]