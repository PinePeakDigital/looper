# Static analysis report

- scope: **whole-repo**  ·  target: `/Users/narthur/code/looper`
- total findings: **11**

## Tools

| tool | findings | exit |
|---|---|---|
| markdownlint | 4 | 1 |
| zizmor | 4 | 14 |
| semgrep | 3 | 1 |
| actionlint | 0 | 0 |
| gitleaks | 0 | 0 |
| golangci-lint | 0 | 1 |
| govulncheck | 0 | 1 |
| yamllint | 0 | 0 |

### markdownlint (4)
```
markdownlint-cli2 v0.23.3 (markdownlint v0.41.1)
Finding: README.md
Linting: 1 file
Summary: 11 issues in 1 file
```

### zizmor (4)
```
artipacked
excessive-permissions
unpinned-uses
unpinned-uses
```

### semgrep (3)
```
.github/workflows/ci.yml:12 yaml.github-actions.security.github-actions-mutable-action-tag.github-actions-mutable-action-tag
.github/workflows/ci.yml:13 yaml.github-actions.security.github-actions-mutable-action-tag.github-actions-mutable-action-tag
internal/mutate/mutate.go:137 go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
```
