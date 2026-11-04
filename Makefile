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
# Runs through npm 11 because npm 10 (bundled with Node 22) fails on a
# full-length SHA in a git spec with "GitFetcher requires an Arborist
# constructor". Drop the npm@11 wrapper once every machine is on npm 11.
AGENT_SKILLS = npx -y npm@11 exec -y --package=github:cwinters8/agent-skills\#1f8e33d2ccf1524b500949b9d29c8f2a2b29aa3b -- agent-skills

skills-sync:
	$(AGENT_SKILLS) sync

skills-check:
	$(AGENT_SKILLS) sync --check
