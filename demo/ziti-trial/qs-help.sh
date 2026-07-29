#!/usr/bin/env bash
ZITI=/c/Users/claude/.ziti/bin/v2.0.0/ziti.exe
"$ZITI" edge quickstart --help > /tmp/qs.txt 2>&1
cat /tmp/qs.txt
