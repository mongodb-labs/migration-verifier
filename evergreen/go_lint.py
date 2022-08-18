import subprocess

subprocess.run("go get -d ./...", shell=True, check=True)
# TODO: this can't be right, maybe someone on evergreen can correct this
subprocess.run(
    "curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b .",
    shell=True,
    check=True)
subprocess.run("./golangci-lint run", shell=True, check=True)
