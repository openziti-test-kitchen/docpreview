#!/usr/bin/env bash
# Probe the ziti CLI image for the quickstart flags we need.
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

run() {
  echo "=== $* ==="
  docker run --rm openziti/ziti-cli:latest "$@" > /tmp/out.txt 2> /tmp/err.txt
  echo "exit=$?"
  echo "--- stdout ---"
  head -40 /tmp/out.txt
  echo "--- stderr ---"
  head -40 /tmp/err.txt
  echo
}

run version
run edge quickstart --help
