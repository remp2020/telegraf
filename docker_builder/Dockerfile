FROM golang:1.21

RUN mkdir -p /src/build

RUN mkdir -p /go/src/github.com/influxdata/telegraf

WORKDIR /go/src/github.com/influxdata/telegraf

COPY build /usr/local/bin/

RUN chmod +x /usr/local/bin/build

CMD ["build"]
