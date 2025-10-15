.PHONY: $(MAKECMDGOALS)

build: webapi cron tool

mkbin:
	@mkdir -p bin/

webapi: mkbin genapitypes
	go build -o bin/spade-webapi ./webapi

genapitypes:
	go generate ./apitypes/apierrors.go

cron: mkbin genfiltypes
	go build -o bin/spade-cron ./cron

tool: mkbin gentypes
	go build -o bin/spade-tool ./tool

gentypes: genfiltypes genapitypes

genfiltypes:
	go generate ./internal/filtypes/types.go