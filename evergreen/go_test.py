import os
import subprocess

try:
    mongodb_distro = "mongodb-" + os.environ["OS"] + "-" + os.environ[
        "ARCH"] + "-" + os.environ["PLATFORM"]
except:
    print("Failed to get required options needed to run the test")
    raise
else:
    print(f"Running go test with MONGODB_DISTRO={mongodb_distro}")

env_with_distro = os.environ.copy()
env_with_distro["MONGODB_DISTRO"] = mongodb_distro

subprocess.run("go test -v ./...", shell=True, check=True, env=env_with_distro)
