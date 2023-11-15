.PHONY: test
test:
	go test -tags=assert -race ./...

.PHONY: lint
lint:
	golangci-lint run

# clone plugin-pb repo
.PHONY: clone
clone:
	git clone https://github.com/pixisai/loki-plugin-pb

.PHONY: gen-proto
gen-proto:
	protoc --proto_path=. --go_out . --go_opt=module="github.com/pixisai/loki-plugin-pb-go" --go-grpc_out=. --go-grpc_opt=module="github.com/pixisai/loki-plugin-pb-go" discovery/discovery.proto
	protoc --proto_path=. --go_out . --go_opt=module="github.com/pixisai/loki-plugin-pb-go" --go-grpc_out=. --go-grpc_opt=module="github.com/pixisai/loki-plugin-pb-go" base/base.proto destination/destination.proto source/source.proto
	protoc --proto_path=. --go_out . --go_opt=module="github.com/pixisai/loki-plugin-pb-go" --go-grpc_out=. --go-grpc_opt=module="github.com/pixisai/loki-plugin-pb-go" plugin-pb/plugin/plugin.proto
