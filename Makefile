.PHONY: build test test-chat-protocol test-shell test-recovery web reliability docker-up docker-down bootstrap upgrade

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
GO_TEST = CGO_ENABLED=1 go test -ldflags=-linkmode=external
else
GO_TEST = CGO_ENABLED=0 go test
endif

build:
	CGO_ENABLED=0 go build ./cmd/control-plane ./cmd/host-agent

test:
	$(MAKE) test-chat-protocol
	$(GO_TEST) ./...
	$(MAKE) test-shell

test-chat-protocol:
	$(GO_TEST) ./internal/provisioner -run '^TestHermesChatProtocolConformance$$'
	$(GO_TEST) ./internal/store -run '^TestChatProtocol(Replay|Terminal)Conformance$$'

test-shell:
	bash scripts/setup-lock_test.sh
	bash scripts/fleet-entrypoints_test.sh
	bash scripts/setup-lib_test.sh
	bash scripts/release-catalog-compose_test.sh
	bash scripts/assert-compose-owner_test.sh
	bash scripts/fleet-edge-network_test.sh
	bash scripts/host-agent-bootstrap_test.sh
	bash scripts/install-launchd-agent_test.sh
	bash scripts/systemd-agent-unit_test.sh
	bash scripts/systemd-control-plane-watchdog-unit_test.sh
	bash scripts/control-plane-upgrade_test.sh
	bash runtime/entrypoint_test.sh

test-recovery:
	@test -n "$(HERMES_FLEET_RECOVERY_TEST_IMAGE)" || (echo "Set HERMES_FLEET_RECOVERY_TEST_IMAGE to a local image containing sh and tar." >&2; exit 1)
	HERMES_FLEET_RECOVERY_INTEGRATION=1 HERMES_FLEET_RECOVERY_TEST_IMAGE="$(HERMES_FLEET_RECOVERY_TEST_IMAGE)" $(GO_TEST) ./internal/provisioner -run '^TestDockerRecoveryVolumeRoundTrip$$'

web:
	cd web && npm run build

reliability:
	bash scripts/reliability-qualification.sh

docker-up:
	bash scripts/assert-compose-owner.sh "$(CURDIR)"
	docker compose up -d --build

docker-down:
	./scripts/fleet-stop.sh

bootstrap:
	./scripts/fleet-bootstrap.sh

upgrade:
	./scripts/fleet-upgrade.sh
