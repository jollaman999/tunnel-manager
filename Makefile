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
# matching libc on the machine it lands on and can be built from any host. It
# also writes SHA256SUMS, which goes up with the binaries so a download can be
# checked. The names in it carry no directory, so the checking works wherever
# the files are put. macOS has shasum instead of sha256sum, and both write the
# same format.
release: clean
	@files=; \
	for p in $(RELEASE_PLATFORMS); do \
		os=$${p%%/*}; arch=$${p##*/}; ext=; \
		if [ "$$os" = windows ]; then ext=.exe; fi; \
		out=$(APP_NAME)-$$os-$$arch$$ext; \
		echo "GOOS=$$os GOARCH=$$arch go build -o $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -o $$out . || exit 1; \
		files="$$files $$out"; \
	done; \
	if command -v sha256sum >/dev/null 2>&1; then \
		sha256sum $$files > SHA256SUMS; \
	elif command -v shasum >/dev/null 2>&1; then \
		shasum -a 256 $$files > SHA256SUMS; \
	else \
		echo "no sha256sum and no shasum: SHA256SUMS cannot be written" >&2; \
		exit 1; \
	fi; \
	echo "wrote SHA256SUMS"

clean:
	rm -f $(APP_NAME) $(APP_NAME)-linux-* $(APP_NAME)-darwin-* $(APP_NAME)-windows-* SHA256SUMS

run-test-server:
	$(MAKE) -C test-server/httpMultiPort run
