#!/bin/bash

ROOT_DIR="$(dirname "$0")/.."

cd "${ROOT_DIR}" || exit 1

TEST_DIR="${1:-build/tests}"
BUILD_CONFIG="${2:-}"

args=(--test-dir "${TEST_DIR}" -LE "NotWorking|RequiresApp" --no-tests=error --output-junit "$(pwd)/ctest_results.xml" --output-on-failure -j)
[ -n "${BUILD_CONFIG}" ] && args+=(--build-config "${BUILD_CONFIG}")
ctest "${args[@]}"
