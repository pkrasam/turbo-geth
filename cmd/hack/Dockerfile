# Build Geth in a stock Go builder container
FROM golang:1.14-alpine as builder

RUN apk add --no-cache make gcc musl-dev linux-headers git ca-certificates

WORKDIR /app
VOLUME /chaindata/
VOLUME /app/

# next 2 lines helping utilize docker cache
COPY go.mod go.sum /app/
RUN go mod download

ADD . /app
RUN make hack

RUN apk add --no-cache
COPY --from=builder /app/build/bin/hack /usr/local/bin/

VOLUME /chaindata/
VOLUME /app

CMD hack --action=lmdbStateAttack --chaindata=/chaindata

