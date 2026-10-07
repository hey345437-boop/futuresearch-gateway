BIN := fsgw

.PHONY: build run test fmt vet docker clean

MODULE := $(shell head -1 go.mod | awk '{print $$2}')
STAMP  := $(shell date +%m%d-%H%M)

build: ## 本地构建（带面板构建戳）
	CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X $(MODULE)/internal/server.BuildStamp=$(STAMP)" \
		-o $(BIN) ./cmd/gateway

run: build ## 起一个本地实例（默认 127.0.0.1:7868）
	./$(BIN) --config config.json

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

docker: ## 构建镜像
	docker build -t futuresearch-gateway:latest .

clean:
	rm -f $(BIN)
