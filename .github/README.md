<p align="center">
  <img src="https://raw.githubusercontent.com/pasadizo-net/.github/main/assets/banner.png" alt="Pasadizo, a secret passage for your whole network" width="100%">
</p>

<p align="center">
  <a href="https://github.com/pasadizo-net/sing-quic/actions/workflows/pasadizo-ci.yml"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/pasadizo-net/sing-quic/pasadizo-ci.yml?branch=main&label=ci"></a>
  <a href="https://github.com/pasadizo-net/sing-quic/actions/workflows/pasadizo-sync.yml"><img alt="Upstream sync" src="https://img.shields.io/github/actions/workflow/status/pasadizo-net/sing-quic/pasadizo-sync.yml?branch=main&label=upstream%20sync"></a>
  <a href="../LICENSE"><img alt="License: GPL-3.0-or-later" src="https://img.shields.io/badge/license-GPL--3.0--or--later-1E1B4B"></a>
  <a href="https://github.com/SagerNet/sing-quic"><img alt="Based on sing-quic" src="https://img.shields.io/badge/based%20on-sing--quic-64748B"></a>
</p>

# sing-quic for Pasadizo

This repository is [sing-quic](https://github.com/SagerNet/sing-quic), the library with the Hysteria, Hysteria2 and TUIC implementations that sing-box uses, with the changes that [Pasadizo](https://github.com/pasadizo-net/pasadizo) needs in it. It is upstream's history of the `dev` branch, and each change is one commit on top of it.

**This repository is not affiliated with sing-quic, sing-box or their authors**, and it is not endorsed by them. Report problems with this build here, never upstream.

## The changes

| Change | Files |
| --- | --- |

None yet: `main` is upstream's `dev` at the commit that Pasadizo pins, plus the workflows and this file. Each change is added by a pull request and listed here.

Changes go into new files where that is possible, so that a merge of upstream seldom conflicts, and each is written so that it can be offered upstream as it is.

## How Pasadizo uses it

The module path is still `github.com/sagernet/sing-quic`, so no import changes anywhere. Pasadizo's `go.mod` takes this repository with a `replace`:

```
replace github.com/sagernet/sing-quic => github.com/pasadizo-net/sing-quic <version of a commit of main>
```

Pasadizo's `require` line keeps the version that sing-box pins. That commit must be an ancestor of the commit the `replace` names: otherwise Pasadizo would build with older QUIC code than sing-box asks for, and nothing would fail. Pasadizo's CI checks it.

There are no releases and no tags of this repository's own: a Go pseudo-version of a commit of `main` is enough for the `replace`.

## Staying in sync with upstream

`.github/workflows/pasadizo-sync.yml` runs every Monday, before Pasadizo's own sync. When upstream's `dev` has new commits, it merges them into a branch `sync/<commit>`, vets, builds and tests the merge, and opens a pull request against `main`; on a merge conflict it opens an issue. Nothing reaches `main` without a person merging the pull request.

The job that runs upstream's code has a read-only token. A second job, which runs no code from the repository, repeats the merge and opens the pull request.

Upstream's `lint.yml` and `debug.yml` workflows are removed here, because they run on the branch name `main`, which this repository also uses. When upstream changes one of them, the sync reports a conflict in that file; resolve it by keeping the file deleted.

## License

GPL-3.0-or-later, as upstream. See [LICENSE](../LICENSE).
