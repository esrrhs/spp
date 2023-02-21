FROM golang AS build-env

RUN go get -u github.com/esrrhs/spp
RUN go mod tidy
RUN go install github.com/esrrhs/spp

FROM debian
COPY --from=build-env /go/bin/spp .
WORKDIR ./
