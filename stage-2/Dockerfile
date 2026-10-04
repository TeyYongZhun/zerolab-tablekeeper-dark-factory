FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -o /tablekeeper .

FROM alpine:3.20
COPY --from=build /tablekeeper /usr/local/bin/tablekeeper
ENV PORT=8080
EXPOSE 8080
CMD ["tablekeeper"]
