.PHONY: test lint validate run image worker-image smoke deploy

test:
	go vet ./...
	go test -race ./...

lint:
	golangci-lint run ./...

validate:
	go run ./cmd/chuck validate

# Runs the daemon locally without Litestream; state goes to ./data.
run:
	CHUCK_HEARTBEAT=5s go run ./cmd/chuck run

image:
	docker build -t chuck:dev --build-arg VERSION=$$(git describe --always --dirty 2>/dev/null || echo dev) .

worker-image:
	docker build -f deploy/worker.Dockerfile -t chuck-worker:dev .

smoke: image
	deploy/smoke-restore.sh chuck:dev

deploy:
	fly deploy . --config deploy/fly.toml --dockerfile Dockerfile
