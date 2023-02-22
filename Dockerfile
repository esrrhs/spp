FROM golang AS build-env

WORKDIR /app

COPY go.* ./
RUN go mod download
RUN go build -v -o spp

FROM debian
COPY --from=build-env /app/spp .
WORKDIR ./
