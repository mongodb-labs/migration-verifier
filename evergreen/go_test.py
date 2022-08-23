import subprocess

subprocess.run("go test -v ./...", shell=True, check=True)
