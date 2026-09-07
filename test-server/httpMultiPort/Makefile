.PHONY: all build clean

APP_NAME := httpMultiPort

all: clean build

build:
	CGO_ENABLED=0 go build -o $(APP_NAME) main.go

run : build
	./$(APP_NAME)

clean:
	rm -f $(APP_NAME)