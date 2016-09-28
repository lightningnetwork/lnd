FROM golang:1.6

MAINTAINER Andrew Samokhvalov <andrew.shvv@gmail.com>

# Create env variable which holds working directory path
ENV APP "$GOPATH/src/github.com/btcsuite/btcwallet"

RUN mkdir -p "$APP"
WORKDIR "$APP"

# Download latest version of `btcwallet` and `glide`
RUN git clone --depth=1 https://github.com/btcsuite/btcwallet.git .
RUN go get -v github.com/Masterminds/glide

RUN glide install && go install . ./cmd/...

# Expose RPC port
EXPOSE "18554"

# Copy start script to work directory
COPY "./start-btcwallet.sh" .
