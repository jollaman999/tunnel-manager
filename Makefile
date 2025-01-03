.PHONY: all build clean ssh

APP_NAME := tunnel-manager

all: clean ssh build

build:
	CGO_ENABLED=0 go build -o $(APP_NAME) main.go

clean:
	rm -f $(APP_NAME)