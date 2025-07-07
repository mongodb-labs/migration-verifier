# Releasing Migration Verifier

To release Migration Verifier, just create a lightweight git tag, thus:
```
git tag v0.0.2
```
… and push it to upstream.

Versions **MUST** start with a `v` and follow
[semantic versioning](https://semver.org/).

An automated release process will build binaries and make them available
for download. Check GitHub Actions for progress.

Note that this process *DOES NOT* create release notes. Users should peruse
the repository’s commit history for changes.
