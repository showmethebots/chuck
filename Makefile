.PHONY: test lint validate run image worker-image smoke deploy skills-sync skills-check

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

# The one place the agent-skills ref lives. Bump it with skills-adopt's
# "Bumping the pinned ref" steps, then run skills-sync.
AGENT_SKILLS = npx -y github:cwinters8/agent-skills\#d311c8176584bea27bd17e9c371d46f9bfde072f

skills-sync:
	$(AGENT_SKILLS) sync

skills-check:
	$(AGENT_SKILLS) sync --check
