# Nexus-Setup Agent Guidelines

## Branches, Commits, and Releases
- `main` is protected and always shippable. Work on a short-lived branch (`feature/…`, `fix/…`,
  `chore/…`) and open a pull request; direct pushes to `main` are rejected.
- Pull requests are squash-merged, so the PR title becomes the commit on `main` and the line in the
  release notes. Write it in the present tense and describe the user-visible change.
- Only open a pull request for finished work. Unfinished features stay on their branch or behind a
  config flag so `main` can be released at any time.
- Commit as the repository's configured git identity (Yosodog). Do not change `user.name` or
  `user.email`, and do not add `Co-Authored-By` or other attribution trailers.
- Releases are cut from `main` by the maintainer: `vX.Y.Z` for stable and `vX.Y.Z-beta.N` for
  prereleases (patch for fixes, minor for features, major for changes that break another Nexus
  component). Do not create tags or GitHub releases.
