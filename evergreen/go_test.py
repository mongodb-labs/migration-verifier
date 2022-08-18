import subprocess

subprocess.run("go get -d ./...", shell=True, check=True)
subprocess.run("go test -v ./...", shell=True, check=True)
