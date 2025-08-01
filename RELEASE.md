# Releasing Migration Verifier

To release Migration Verifier, create an annotated git tag, thus:
```
git tag --annotate v0.0.2
```
The tag contents will be the release notes. See prior releases for the format.
Once you’re done, push your tag to upstream. A pre-configured GitHub Action
will create the release.

**IMPORTANT:** Versions **MUST** start with a `v` and follow
[semantic versioning](https://semver.org/).
