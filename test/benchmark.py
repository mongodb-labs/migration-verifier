import argparse
import datetime
import json
import os
import shutil
import sys
import threading
import time
from pymongo import MongoClient
import subprocess
import multiprocessing
from dateutil.tz import tzutc

parser = argparse.ArgumentParser()
parser.add_argument(
    "--workers", "-w", type=int, default=multiprocessing.cpu_count())
parser.add_argument("--partitionSizeMB", "-p", type=int)
parser.add_argument(
    "--way", required=True, type=str, choices=["check", "recheck"])
parser.add_argument("--no_clean", default=False, action='store_true')
subparsers = parser.add_subparsers(dest="mode")
local_parser = subparsers.add_parser("local")

local_parser.add_argument(
    "--mongod",
    "-m",
    type=str,
    required=True,
    default=
    "internal/verifier/mongodb_exec/mongodb-linux-x86_64-ubuntu1804-6.0.1/bin/mongod"
)

remote_parser = subparsers.add_parser("remote")
remote_parser.add_argument(
    "--namespace", "-n", nargs='+', type=str,
    default=["sampleDb.col1"])  #, "sampleDb.col2", "sampleDb.col3"])
remote_parser.add_argument(
    "--recheck_namespace", "-r", type=str, default="sampleDb.recheck_col")
remote_parser.add_argument(
    "--meta-uri",
    "-m",
    type=str,
    required=True,
)
remote_parser.add_argument(
    "--src-uri",
    "-s",
    type=str,
    required=True,
)
remote_parser.add_argument(
    "--dst-uri",
    "-d",
    type=str,
    required=True,
)
args = parser.parse_args()


def run_mongod(port, path):
    subprocess.run([
        args.mongod, "--port", port, "--dbpath", path, "--logpath",
        f"{path}/log.log"
    ])


def insert_docs(client: MongoClient, num: int):
    db = client.example_db
    coll = db.example_collection
    for i in range(num):
        coll.insert_one({"example_doc": 1, "_id": i})
    cursor = coll.find({})
    for document in cursor:
        print(document)


def start_check_by_type(check_type: str):
    while True:
        proc = subprocess.run(
            "curl -H \"Content-Type: application/json\" -X POST -d '{}' http://127.0.0.1:27020/api/v1/"
            + check_type,
            shell=True)
        if proc.returncode == 0:
            return
        print("Failed to run curl command")
        time.sleep(1)


def start_check():
    print("starting check")
    return start_check_by_type("check")


def start_recheck():
    print("starting recheck")
    return start_check_by_type("writesOn")


def get_status():
    while True:
        time.sleep(1)
        proc = subprocess.run(
            "curl -H \"Content-Type: application/json\" -X GET http://127.0.0.1:27020/api/v1/progress",
            shell=True,
            capture_output=True)
        try:
            status = json.loads(proc.stdout.decode('UTF-8'))
        except:
            print("Failed to get status")
            print(sys.exc_info())
            continue

        print(json.dumps(status, sort_keys=True, indent=4))

        if status["progress"]["error"]:
            raise "Woops we had an error"
        if status["progress"]["phase"] == "idle":
            return
        time.sleep(9)


def run_verifier(command: str):
    print(command)
    subprocess.run(command, shell=True)


if args.mode == "local":
    shutil.rmtree("dbpath1", ignore_errors=True)
    shutil.rmtree("dbpath2", ignore_errors=True)
    os.makedirs("dbpath1", exist_ok=True)
    os.makedirs("dbpath2", exist_ok=True)

    src_port = 27001
    dst_port = 27002

    srcMongo = threading.Thread(
        target=run_mongod, args=(f"{src_port}", "dbpath1"))
    dstMongo = threading.Thread(
        target=run_mongod, args=(f"{dst_port}", "dbpath2"))
    srcMongo.start()
    dstMongo.start()

    srcClient = MongoClient(host="localhost", port=src_port)
    dstClient = MongoClient(host="localhost", port=dst_port)

    insert_docs(srcClient, 2)
    insert_docs(dstClient, 1)

    meta_uri = f"mongodb://localhost:{dst_port}"
    src_uri = f"mongodb://localhost:{src_port}"
    dst_uri = f"mongodb://localhost:{dst_port}"
    namespaces = [f"example_db.example_collection"]
else:
    srcClient = MongoClient(args.meta_uri)
    print(srcClient.list_database_names())
    print(srcClient)
    meta_uri = args.meta_uri
    src_uri = args.src_uri
    dst_uri = args.dst_uri
    namespaces = args.namespace

    # go build main/migration_verifier.go

subprocess.run(["go", "build", "main/migration_verifier.go"], check=True)
# command = [
#     "go", "test", "-v", "-run", "^$", "-bench", "^BenchmarkGeneric$",
#     "-benchtime", "1x", "-timeout", "5h",
#     "github.com/mongodb-labs/migration-verifier/internal/verifier"
# ]

src_namespaces = ' '.join([
    "--srcNamespace " + namespace
    for namespace in namespaces + [args.recheck_namespace]
])
dst_namespaces = ' '.join([
    "--dstNamespace " + namespace
    for namespace in namespaces + [args.recheck_namespace]
])
partitionSizeMBArg = f"--partitionSizeMB {args.partitionSizeMB}" if args.partitionSizeMB else ""
clearArg = '' if args.way == "recheck" else '--clean'
# clearArg = '--clean'
command = f"./migration_verifier --srcURI \"{src_uri}\" --dstURI \"{dst_uri}\" --metaURI \"{meta_uri}\" {partitionSizeMBArg} --numWorkers {str(args.workers)} {src_namespaces} {dst_namespaces} {clearArg} --debug"

srcClient = MongoClient(src_uri)
dstClient = MongoClient(dst_uri)
DOCS_TO_INSERT = 1000000
INSERT_MANY_SIZE = 10000

MIN_ID = 128478209
MAX_ID = 128478209 + DOCS_TO_INSERT

recheckdb = args.recheck_namespace.split('.')[0]
recheckcol = args.recheck_namespace.split('.')[1]
if not args.no_clean:
    # Clear out data run by an old benchmark
    print(srcClient[recheckdb][recheckcol].delete_many({}).raw_result)
    print(dstClient[recheckdb][recheckcol].delete_many({}).raw_result)

run_verifier_thread = threading.Thread(target=run_verifier, args=(command, ))
run_verifier_thread.start()

if args.way == "check":
    start_check()
    get_status()

#     {
#     "progress": {
#         "error": null,
#         "phase": "check",
#         "verificationStatus": {
#             "addedTasks": 684,
#             "completedTasks": 112,
#             "failedTasks": 0,
#             "metadataMismatchTasks": 0,
#             "processingTasks": 128,
#             "recheckTasks": 0,
#             "totalTasks": 924
#         }
#     }
# }

elif args.way == "recheck":
    start_recheck()
    start_check()
    # time.sleep(5)

    #TODO: insert a bunch of data
    for i in range(0, DOCS_TO_INSERT, INSERT_MANY_SIZE):
        # docs = [{
        #     "_id": {
        #         "w": {
        #             "$numberInt": "5"
        #         },
        #         "i": {
        #             "$numberInt": "17"
        #         }
        #     },
        #     "fld0": {
        #         "$numberLong": "163872"
        #     },
        #     "fld1": {
        #         "$date": {
        #             "$numberLong": "1605623989425"
        #         }
        #     },
        #     "fld2":
        #     "sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Loremsed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est",
        #     "fld3":
        #     "takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eostakimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero",
        #     "fld4":
        #     "labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sitlabore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum",
        #     "fld5": {
        #         "$date": {
        #             "$numberLong": "1294020930094"
        #         }
        #     },
        #     "fld6": {
        #         "$numberLong": "668793"
        #     },
        #     "fld7":
        #     "est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justoest Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam",
        #     "fld8":
        #     "ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stetipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stetipsum",
        #     "fld9": {
        #         "$numberLong": "456404"
        #     },
        #     "bin": {
        #         "$binary": {
        #             "base64": "",
        #             "subType": "00"
        #         }
        #     }
        # } for i in range(INSERT_MANY_SIZE)]
        docs = [{
            "_id": {
                "w": i + j,
                "i": i + j
            },
            "fld0":
            163872,
            "fld1":
            datetime.datetime(2017, 10, 13, 10, 53, 53, tzinfo=tzutc()),
            "fld2":
            "sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Loremsed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est",
            "fld3":
            "takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eostakimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero",
            "fld4":
            "labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sitlabore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum",
            "fld5":
            datetime.datetime(2017, 10, 13, 10, 53, 53, tzinfo=tzutc()),
            "fld6":
            668793,
            "fld7":
            "est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justoest Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam",
            "fld8":
            "ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stetipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stet clita kasd gubergren, no sea takimata sanctus est Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet, consetetur sadipscing elitr, sed diam nonumy eirmod tempor invidunt ut labore et dolore magna aliquyam erat, sed diam voluptua. At vero eos et accusam et justo duo dolores et ea rebum. Stetipsum",
            "fld9":
            456404,
        } for j in range(INSERT_MANY_SIZE)]

        srcClient[recheckdb][recheckcol].insert_many(docs)
        dstClient[recheckdb][recheckcol].insert_many(docs)
        print(round(i / DOCS_TO_INSERT, 2))

    subprocess.run(
        "curl -H \"Content-Type: application/json\" -X POST -d '{}' http://127.0.0.1:27020/api/v1/writesOff",
        shell=True)

    # We we finish get status we can run recheck
    get_status()

run_verifier_thread.join()

# "MONGODB_DISTRO=mongodb-linux-x86_64-ubuntu1804 go test -v -bench=. ./..."