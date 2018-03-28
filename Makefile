.PHONY: all glidebin glide deps install fmt test

all: install

glidebin:
	go get -u github.com/Masterminds/glide

glide: glidebin
	glide install

deps: glide

install: deps
	go install . ./cmd/...

fmt:
	go fmt ./... $$(go list ./... | grep -v '/vendor/')

test:
	go test -v -p 1 $$(go list ./... | grep -v '/vendor/')
