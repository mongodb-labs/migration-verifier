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

To check all namespaces:
    ./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --metaURI mongodb://127.0.0.1:27001 --metaDBName verify_meta

To filter namespaces (allow list):
    ./migration_verifier --srcURI mongodb://127.0.0.1:27002 --dstURI mongodb://127.0.0.1:27003 --metaURI mongodb://127.0.0.1:27001 --metaDBName verify_meta --srcNamespace foo.bar --srcNamespace foo.car
