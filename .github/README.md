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
| `(*hysteria2.Client).ConnectionStats()` returns the statistics of the client's QUIC connection (round trip times, congestion window, bytes and packets sent, received and lost), and whether it has an open connection at all. Pasadizo reads it to tell a dead path from a slow destination without sending test traffic. | `hysteria2/client_stats.go`, `hysteria2/client_stats_test.go` |
| Careful Resume ([RFC 9959](https://www.rfc-editor.org/rfc/rfc9959)) for the BBR sender: `hysteria2.ClientOptions.CarefulResume` and `hysteria2.ServiceOptions.CarefulResume` take a `congestion_meta2.CarefulResumeStore`, and a new connection to a peer that an earlier connection measured starts from half of the saved bottleneck bandwidth, once the path is confirmed, instead of from slow start. Nil (the default) changes nothing. The design, the one deviation from the RFC and the measurements are in the README of [pasadizo-net/hysteria](https://github.com/pasadizo-net/hysteria) (pull request #9), which has the same code. | `congestion_meta2/careful_resume*.go`, `congestion_meta2/bbr_sender.go`, `hysteria2/client.go`, `hysteria2/service.go` |
| Hysteria2 over several network paths with the multipath extension of QUIC ([RFC 10069](https://www.rfc-editor.org/rfc/rfc10069)): `ClientOptions.MultipathPaths` adds paths (a dialer each, for example bound to another interface, optionally a backup path and a Brutal bandwidth of its own) that the client keeps open on its connection and opens again when they close; `ServiceOptions.Multipath` accepts them. Each path gets a congestion controller of its own: BBR, or Brutal only on a client path that has a bandwidth of its own, since Brutal at the connection's rate on every path would send it several times over; Careful Resume applies to the first path only. It needs [pasadizo-net/quic-go](https://github.com/pasadizo-net/quic-go), which `go.mod` takes with a `replace` of `github.com/sagernet/quic-go`; a module that uses this repository needs the same `replace`, since Go ignores the `replace` directives of dependencies. | `hysteria2/multipath.go`, `hysteria2/multipath_test.go`, `hysteria2/client.go`, `hysteria2/service.go`, `go.mod` |

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
