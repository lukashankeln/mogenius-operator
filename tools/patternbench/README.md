# How to use

## Installation prerequisites

`main.go` uses the cli tool [hyperfine](https://github.com/sharkdp/hyperfine) to benchmark requests. Check the projects README.md for install instructions. On Mac `brew install hyperfine` can be used.

The python scripts are taken from the Hyperfine repository. They generate summaries/graphs from the collected data. These scripts require `matplotlib` and `numpy`.

```sh
pip install numpy
pip install matplotlib
```

## Enable collection of `patternlogs.jsonl`

1. Export the ENV variable `MO_ENABLE_PATTERNLOGGING` with any value.
2. Start the operator with `just run`. Use the platform website and navigate around to trigger patterns. `patternlogs.jsonl` will contain all calls to patterns in the patternhandler.

The file holds every payload unredacted, including secrets a request carries (the `env` of exec and session requests). Use it on a local operator only and delete it afterwards.

## Generate Summary

Run `tools/patternbench/main.go` from the Git repositories root.

```sh
go run tools/patternbench/main.go
```
