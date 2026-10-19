#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

export TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://jungle_test:jungle_test@localhost:55432/jungle_test?sslmode=disable}"
test_pid=

compose() {
    docker compose -p jungle-tests -f docker-compose.test.yml "$@"
}

cleanup() {
    status=$?
    trap - EXIT INT TERM

    if [ -n "$test_pid" ]; then
        kill -TERM "$test_pid" 2>/dev/null || true
        wait "$test_pid" 2>/dev/null || true
    fi

    if [ "$status" -ne 0 ]; then
        compose logs --no-color --tail=100 || true
        printf '%s\n' \
            'Mantive os containers de teste para investigação.' \
            'Logs: docker compose -p jungle-tests -f docker-compose.test.yml logs -f' \
            'Ao terminar: make test-deps-down' >&2
        exit "$status"
    fi

    if ! compose down --timeout 10; then
        printf '%s\n' 'Não foi possível encerrar os containers de teste.' >&2
        if [ "$status" -eq 0 ]; then
            status=1
        fi
    fi
    exit "$status"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

compose up -d --wait

go test -race -tags=integration ./tests/integration/... -count=1 \
    "-timeout=${TEST_TIMEOUT:-5m}" "$@" &
test_pid=$!
status=0
wait "$test_pid" || status=$?
test_pid=
exit "$status"
