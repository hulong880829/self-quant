ENV_FILE ?= .env
SHARED_ENV := deploy/env/container.shared.env
SECRETS_DIR := deploy/secrets
COMPOSE_FILE := deploy/docker-compose.yml

.PHONY: init-auth-secrets prepare-compose-env \
	bootstrap-compose bootstrap-compose-new bootstrap-compose-upgrade \
	bootstrap-compose-restore validate-compose

init-auth-secrets:
	@test -n "$(MODE)" || (echo "set MODE=new|upgrade|restore; prefer make bootstrap-compose" >&2; exit 1)
	@bash deploy/scripts/init-auth-secrets.sh --mode "$(MODE)" --env-file "$(ENV_FILE)" --secrets-dir "$(SECRETS_DIR)"

prepare-compose-env:
	@test -f "$(ENV_FILE)" || (echo "missing $(ENV_FILE); create it from .env.example" >&2; exit 1)
	@bash deploy/scripts/prepare-compose-env.sh "$(ENV_FILE)" "$(SHARED_ENV)"

bootstrap-compose:
	@bash deploy/scripts/bootstrap-compose.sh --mode auto --env-file "$(ENV_FILE)" --secrets-dir "$(SECRETS_DIR)" --compose-file "$(COMPOSE_FILE)"

bootstrap-compose-new:
	@bash deploy/scripts/bootstrap-compose.sh --mode new --env-file "$(ENV_FILE)" --secrets-dir "$(SECRETS_DIR)" --compose-file "$(COMPOSE_FILE)"

bootstrap-compose-upgrade:
	@bash deploy/scripts/bootstrap-compose.sh --mode upgrade --env-file "$(ENV_FILE)" --secrets-dir "$(SECRETS_DIR)" --compose-file "$(COMPOSE_FILE)"

bootstrap-compose-restore:
	@bash deploy/scripts/bootstrap-compose.sh --mode restore --env-file "$(ENV_FILE)" --secrets-dir "$(SECRETS_DIR)" --compose-file "$(COMPOSE_FILE)"

validate-compose:
	@bash deploy/scripts/validate-compose-env.sh --level final --env-file "$(ENV_FILE)" --compose-file "$(COMPOSE_FILE)" --shared-env "$(SHARED_ENV)" --secrets-dir "$(SECRETS_DIR)"
