.PHONY: test lint validate run image worker-image smoke deploy

test:
	go vet ./...
	go test -race ./...

lint:
	golangci-lint run ./...

validate:
	go run ./cmd/uncharles validate

# Runs the daemon locally without Litestream; state goes to ./data.
run:
	UNCHARLES_HEARTBEAT=5s go run ./cmd/uncharles run

image:
	docker build -t uncharles:dev --build-arg VERSION=$$(git describe --always --dirty 2>/dev/null || echo dev) .

worker-image:
	docker build -f deploy/worker.Dockerfile -t uncharles-worker:dev .

smoke: image
	deploy/smoke-restore.sh uncharles:dev

deploy:
	fly deploy . --config deploy/fly.toml --dockerfile Dockerfile
