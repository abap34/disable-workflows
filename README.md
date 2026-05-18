# disable-workflows

A terminal UI for finding GitHub Actions workflows across repositories and disabling selected workflows.

## Quick Usage

Scan repositories owned by the authenticated GitHub user:

```sh
disable-workflows
```

Scan an organization or another owner:

```sh
disable-workflows --owner my-org
```

## Install

```sh
go install github.com/abap34/disable-workflows@latest
```

The binary is installed under `$GOBIN` or `$GOPATH/bin`. Make sure that directory is in your `PATH`.

## Authentication

If you already use GitHub CLI, no extra token setup is usually needed:

```sh
gh auth status
disable-workflows
```

Token lookup order:

1. The environment variable named by `--token-env` (`GH_TOKEN` by default)
2. `GH_TOKEN`
3. `GITHUB_TOKEN`
4. `gh auth token`

For fine-grained tokens, grant repository read access and Actions write access for the repositories you want to manage.

## TUI Controls

- `up/down` or `j/k`: move cursor
- `space` or `enter`: select an active workflow
- `a`: select all visible active workflows
- `u`: clear selection
- `/`: filter
- `1`-`7`: sort by visible table columns; press the same number again to flip order
- `[` / `]`: cycle sort column
- `o`: flip sort order
- `d`: disable selected workflows
- `r`: refresh
- `q`: quit

Before sending write requests, the tool asks you to type `disable`.

## Command Line

Common options:

```sh
disable-workflows --owner my-org
disable-workflows --owner my-org --repo api
disable-workflows --owner my-org --max-repos 50
disable-workflows --owner my-org --concurrency 1
disable-workflows --owner my-org --include-archived
```

Last-run lookup:

```sh
disable-workflows --owner my-org --last-run=workflow
disable-workflows --owner my-org --last-run=repo
disable-workflows --owner my-org --last-run=off
```

`workflow` is the default and asks GitHub for the latest run of each workflow.
`repo` is faster, but approximate: it only inspects the repository's most recent workflow runs.

Cache and request pacing:

```sh
disable-workflows --owner my-org --cache-max-age=5m
disable-workflows --owner my-org --cache-max-age=0
disable-workflows --owner my-org --min-request-interval=500ms
```

## License

MIT License. See [LICENSE](LICENSE).
