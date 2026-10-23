# looper — moved

looper now lives in [narthur/skills](https://github.com/narthur/skills), at
[`packages/looper`](https://github.com/narthur/skills/tree/main/packages/looper), next to the
`review-loop` skill it serves. Its full history came with it, so commit shas from this repo
resolve there.

This repo is archived. Its tags stay, so `go install github.com/pinepeakdigital/looper/cmd/looper@v0.2.12`
still works, but nothing newer will be released here. review-loop now builds looper from the
skills checkout:

```bash
cd ~/code/skills/packages/looper && GOBIN=~/.local/bin go install ./cmd/looper
```

The README as it stood at the move is at
[v0.2.12](https://github.com/PinePeakDigital/looper/blob/v0.2.12/README.md).
