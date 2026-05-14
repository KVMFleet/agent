FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=0.1.0" -o /out/kvmfleet-agent ./

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/kvmfleet-agent /usr/local/bin/kvmfleet-agent
ENTRYPOINT ["/usr/local/bin/kvmfleet-agent"]
CMD ["run"]
