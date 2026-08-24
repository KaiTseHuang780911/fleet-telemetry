# The real task definitions live in package.json — this Makefile is a thin
# wrapper so the familiar `make dev` works for anyone on Linux or macOS reading
# the repo. The project is developed on Windows, where GNU make is not present
# by default; npm is, and Node is a dependency of the web and mobile workspaces
# regardless. See "Deviations from the brief" in the README.

.PHONY: dev api sim web build test lint fmt db-setup db-migrate db-reset psql

dev:        ; npm run dev
api:        ; npm run dev:api
sim:        ; npm run dev:sim
web:        ; npm run dev:web
build:      ; npm run build
test:       ; npm test
lint:       ; npm run lint
fmt:        ; npm run fmt
db-setup:   ; npm run db:setup
db-migrate: ; npm run db:migrate
db-reset:   ; npm run db:reset
psql:       ; npm run db:psql
