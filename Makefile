PREFIX ?= $(HOME)/.local
SKILLS ?= $(HOME)/.claude/skills

.PHONY: build test install uninstall

build:
	go build -o rexpad .

test:
	go test ./...

# Installs the binary and links the agent skill so agents pick it up.
install:
	go build -o $(PREFIX)/bin/rexpad .
	mkdir -p $(SKILLS)
	ln -sfn $(CURDIR)/skills/rexpad $(SKILLS)/rexpad

uninstall:
	rm -f $(PREFIX)/bin/rexpad
	rm -f $(SKILLS)/rexpad
