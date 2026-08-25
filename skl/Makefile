# Builds the skl binary into ~/.agents/bin/ (git-ignored).
#
#   make          # build
#   make install  # same as build (binary lands in ~/.agents/bin)
#   make clean    # remove the built binary

BIN := ../bin/skl

.PHONY: all build install clean

all: build

build:
	@mkdir -p ../bin
	go build -o $(BIN) .
	@echo "built $(BIN)"

install: build

clean:
	rm -f $(BIN)
