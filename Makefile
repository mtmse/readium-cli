help:
	@echo "Usage: make <target>\n\n\
	  build\t\tBuild the \`readium\` command-line utility in the current directory\n\
	  install\tBuild and install the \`readium\` command-line utility\n\
	"

.PHONY: build
build:
	go build -o readium ./cmd

.PHONY: install
install:
	(cd cmd; go install)


