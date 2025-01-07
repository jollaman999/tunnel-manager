.PHONY: all build clean ssh

APP_NAME := tunnel-manager

all: clean ssh build

build:
	CGO_ENABLED=0 go build -o $(APP_NAME) main.go

run: build
	sudo ./$(APP_NAME)

clean:
	rm -f $(APP_NAME)

run-test-server:
	$(MAKE) -C test-server/httpMultiPort run
