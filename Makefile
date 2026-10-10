VERSION ?= 0.1.0
SDK_LANGUAGES ?= dotnet nodejs python go
LDFLAGS = -X github.com/jetersen/pulumi-truenas/provider/pkg/version.Version=$(VERSION)
export PATH := $(CURDIR)/bin:$(PATH)

.PHONY: tools schema provider sdk test test-compose-preview
schema:
	mkdir -p bin
	cd provider && go build -ldflags '$(LDFLAGS)' -o ../bin/pulumi-tfgen-truenas ./cmd/pulumi-tfgen-truenas
	./bin/pulumi-tfgen-truenas schema --skip-docs --skip-examples --out provider/cmd/pulumi-resource-truenas

provider: schema
	cd provider && go build -ldflags '$(LDFLAGS)' -o ../bin/pulumi-resource-truenas ./cmd/pulumi-resource-truenas

tools:
	mkdir -p bin
	cd provider && go build -o ../bin/pulumi-language-dotnet github.com/pulumi/pulumi-dotnet/pulumi-language-dotnet/v3
	cd provider && go build -o ../bin/pulumi-language-yaml github.com/pulumi/pulumi-yaml/cmd/pulumi-language-yaml

sdk: schema tools
	@set -e; for language in $(SDK_LANGUAGES); do \
		pulumi package gen-sdk provider/cmd/pulumi-resource-truenas/schema.json --version $(VERSION) --language $$language --out sdk; \
	done
ifneq ($(filter go,$(SDK_LANGUAGES)),)
	cd sdk && go mod tidy
endif

test:
	cd provider && go test ./...

test-compose-preview: provider sdk
	cd provider && VERSION=$(VERSION) TRUENAS_COMPOSE_CLI_TEST=1 go test -run TestComposeCLIPreview -v .
