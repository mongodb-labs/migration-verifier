Verify Migrations!

to build:
    go build main/migration_verifier.go

to run:
    ./migration_verifier --help

Operational UX once running (assumes no port set, default port for operation webserver is 27020)

To do Check:
    curl -H "Content-Type: application/json" -X POST -d '{}' http://127.0.0.1:27020/api/v1/check

To do Recheck (ideally wait until check finishes, definitely stop writes):
    curl -H "Content-Type: application/json" -X POST -d '{}' http://127.0.0.1:27020/api/v1/recheck
