# Energyplan compiled worker

This directory contains the optional proprietary Sourceful Energyplan worker,
its license and third-party notices, and public integration checks. Rust source,
source tests and builds live in the private `srcfl/energyplan` repository.

The executables have a separate license in `bundle/LICENSE.txt`. It permits use
and redistribution of the unmodified workers with FTW, including commercial
FTW distributions. FTW's own source keeps its existing license. Other uses of
the worker require a separate license from Sourceful.

## Verify and run

No Rust toolchain, Python solver package, private repository access or network
service is needed. From the FTW repository root:

```sh
make native-solver-test
python3 optimizer/native/verify.py --host-binary
optimizer/native/bundle/ftw-solver-linux-arm64 --time-limit=100ms < requests.jsonl
```

The bundle contains static Linux ARM64 and Linux AMD64 workers and a macOS
ARM64 worker. The manifest pins their version, private source commit, sizes
and SHA-256 checksums. The verifier checks every bundled file, the host worker
handshake and the public source boundary. Keep the license and notices with
any copied or redistributed executable. Run only a verified bundle.

The worker reads optimizer protocol v1 JSON lines and stays alive across
requests. Core's existing `mpc.ExternalOptimizer` starts it through an absolute
path in `ExternalOptimizerConfig.Command`. Core validates all proposed plans
and keeps its Go fallback. The current settings launcher still starts Python;
this bundle does not select the worker on any site.

Supported requests contain one battery and at most one EV per site, with the
four existing modes, physical limits, negative tariffs and an EV deadline.
Unsupported scenarios, thermal/commercial models and multiple assets return
an error. A time limit can return a feasible plan with a remaining cost gap;
without a feasible candidate it returns a budget error. Core handles errors
through its existing fallback path.

`make verify` includes the binary and integration checks. Go integration tests
can also use an absolute path supplied in `FTW_NATIVE_SOLVER`. Ordinary Go tests
skip these optional process tests when that variable is unset.

## Update the bundle

Build and test a new version in the private repository. Copy only the complete
output of its binary packaging tool into `bundle/`, then run
`make native-solver-test` and `make verify`. Submit the binaries, manifest,
license and notices together. Never add Rust source, Cargo files, source
archives or build tools to this public directory.
