.PHONY: all build update release clean openapi swagger-ui docs docs-check

APP_NAME := tunnel-manager

all: clean build

build:
	CGO_ENABLED=0 go build -o $(APP_NAME) .

run: build
	sudo ./$(APP_NAME)

# update moves every dependency to its newest release and then drops what
# nothing imports any more. tools/demo is a module of its own, with its own
# go.mod, so `go get -u ./...` run here does not reach into it and it is
# updated separately. The generator and the Swagger UI pinned below are not
# dependencies of either module and are moved by hand.
update:
	go get -u ./...
	go mod tidy
	cd tools/demo && go get -u ./... && go mod tidy

# SWAG_VERSION pins the generator. It is written out rather than left at latest
# so that two people who run the target get the same file: a generator that
# moved on its own would show up as a diff nobody made.
SWAG_VERSION := v1.16.6

# OPENAPI_FILE is where the generated description lands. It sits inside the
# directory internal/web embeds, so the file that is committed here is the file
# the binary carries and the file a running server hands out at
# /ui/openapi.json. Three copies that could disagree would be three, and this
# way there is one.
OPENAPI_FILE := internal/web/static/openapi.json

# openapi regenerates the description from the annotations on the handlers. Run
# it after adding, removing or renaming a route; TestTheSpecAndTheRouterAgree
# fails until you do.
#
# The generator is run with `go run <module>@<version>`, which builds and runs
# it in module-aware mode while ignoring the go.mod of this directory. So the
# version above is pinned without go.mod being touched: running `go get` or
# `go install` for it would write the generator and everything it needs into
# go.mod, where a version bump of the tool would then read as a change to what
# this program depends on.
#
# --outputTypes json asks for the description alone. The default also writes a
# docs.go, which is the same description compiled into a Go package. The
# description is already committed as JSON and already built into the binary
# through internal/web, so that file would be a second copy of it that can
# disagree with the first.
#
# --dir lists the packages the types in the annotations are declared in. The
# general information is read from the first one, which is why main.go is named
# separately and this directory is first.
#
# swag names what it writes after itself, so the file is moved to the name it is
# served and documented under. The description is OpenAPI 2.0, which is what
# this generator writes, and every client named in docs/reference.md reads it.
openapi:
	go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION) init \
		--generalInfo main.go \
		--dir .,internal/api,internal/models,internal/settings \
		--outputTypes json \
		--output $(dir $(OPENAPI_FILE)) \
		--overridesFile .swaggo \
		--parseDepth 2 \
		--quiet
	mv $(dir $(OPENAPI_FILE))swagger.json $(OPENAPI_FILE)
	@echo "wrote $(OPENAPI_FILE)"

# The Swagger UI is served from files this repository holds, not from a content
# delivery network. A host that runs this program may reach nothing but the
# machines it forwards to, and a documentation page that only renders for
# someone with internet access is a page the operator who needs it cannot read.
# The same reasoning already put the operator UI inside the binary.
#
# SWAGGER_UI_VERSION pins what is fetched. SWAGGER_UI_INTEGRITY is the hash npm
# publishes for that exact tarball, checked before anything is unpacked, so the
# target either writes the files that were reviewed or writes nothing.
SWAGGER_UI_VERSION := 5.33.0
SWAGGER_UI_INTEGRITY := sha512-wpdK+m6BU5yj6pmUdMskZVTSWYG4DLglAx3sIhylloY37i8O37IrH+YEpqdXNfpaTGxILRBFzUqLF2jKqbfI7A==
SWAGGER_UI_DIR := internal/web/static/api-docs

# Only two files are taken. swagger-ui-standalone-preset.js is left behind: it
# draws the top bar with the box for typing a description URL, and this server
# serves exactly one description. What that bar actually produced was a picker
# with a single entry in it.
#
# The .map files are left behind too. They are read by a browser's debugger
# when someone is working on the Swagger UI itself, which is not something that
# happens here, and they are larger than the code they describe.
SWAGGER_UI_FILES := swagger-ui-bundle.js swagger-ui.css

# swagger-ui downloads the pinned release and writes those files into the
# directory internal/web embeds. Run it to move to a new version of the Swagger
# UI; nothing else needs it, and a build does not.
swagger-ui:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	url=https://registry.npmjs.org/swagger-ui-dist/-/swagger-ui-dist-$(SWAGGER_UI_VERSION).tgz && \
	curl -fsSL "$$url" -o "$$tmp/dist.tgz" && \
	got=sha512-$$(openssl dgst -sha512 -binary "$$tmp/dist.tgz" | openssl base64 -A) && \
	if [ "$$got" != "$(SWAGGER_UI_INTEGRITY)" ]; then \
		echo "the tarball for swagger-ui-dist $(SWAGGER_UI_VERSION) is not the one this Makefile pins"; \
		echo "  want $(SWAGGER_UI_INTEGRITY)"; \
		echo "  got  $$got"; \
		exit 1; \
	fi && \
	mkdir -p $(SWAGGER_UI_DIR) && \
	for f in $(SWAGGER_UI_FILES); do \
		tar -xzf "$$tmp/dist.tgz" -C "$$tmp" "package/$$f" && \
		cp "$$tmp/package/$$f" $(SWAGGER_UI_DIR)/$$f; \
	done && \
	tar -xzf "$$tmp/dist.tgz" -C "$$tmp" package/LICENSE && \
	cp "$$tmp/package/LICENSE" $(SWAGGER_UI_DIR)/LICENSE && \
	echo "wrote $(SWAGGER_UI_DIR) from swagger-ui-dist $(SWAGGER_UI_VERSION)"

# docs is everything the API documentation is made of. It is what to run after
# touching a handler, and what continuous integration runs to find out whether
# someone forgot to.
docs: openapi swagger-ui

# docs-check regenerates and fails if anything moved. A description that no
# longer matches the handlers is already caught by TestTheSpecAndTheRouterAgree,
# but a description that matches the handlers and was never regenerated after a
# field was renamed is not, and neither is a Swagger UI left at an old version.
#
# It reads git status rather than git diff, so that a generated file which was
# never added counts as a file that moved. git diff says nothing at all about a
# file git is not tracking, and the first run of a new generator is exactly the
# case where that file does not exist yet.
docs-check: docs
	@moved=$$(git status --porcelain -- $(OPENAPI_FILE) $(SWAGGER_UI_DIR)) && \
	if [ -n "$$moved" ]; then \
		echo "the generated documentation is not what is committed; run make docs and commit the result"; \
		echo "$$moved"; \
		exit 1; \
	fi
	@echo "the committed documentation matches the sources"

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
