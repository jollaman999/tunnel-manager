.PHONY: all build release clean

APP_NAME := tunnel-manager

all: clean build

build:
	CGO_ENABLED=0 go build -o $(APP_NAME) .

run: build
	sudo ./$(APP_NAME)

# RELEASE_PLATFORMS is what a release is built for. Each entry becomes one file
# whose name carries the platform, so the file says what it runs on.
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# release builds every platform above. CGO is off, so a binary does not need a
# matching libc on the machine it lands on and can be built from any host.
release: clean
	@for p in $(RELEASE_PLATFORMS); do \
		os=$${p%%/*}; arch=$${p##*/}; ext=; \
		if [ "$$os" = windows ]; then ext=.exe; fi; \
		out=$(APP_NAME)-$$os-$$arch$$ext; \
		echo "GOOS=$$os GOARCH=$$arch go build -o $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -o $$out . || exit 1; \
	done

clean:
	rm -f $(APP_NAME) $(APP_NAME)-linux-* $(APP_NAME)-darwin-* $(APP_NAME)-windows-*

run-test-server:
	$(MAKE) -C test-server/httpMultiPort run
