# Contributing to Kocoro

Thanks for your interest in contributing to Kocoro!

## Getting Started

```bash
git clone https://github.com/Kocoro-lab/Kocoro.git
cd Kocoro
go build -o shan .
go test ./...
```

Requires **Go 1.25+**.

## How to Contribute

1. **Bug reports** — open an issue with reproduction steps and expected vs actual behavior
2. **Feature requests** — open an issue describing the use case and proposed solution
3. **Pull requests** — fork the repo, create a branch, and submit a PR

## Pull Request Guidelines

- Keep PRs focused on a single concern
- Include tests for new functionality
- Run `go test ./...` and `go vet ./...` before submitting
- Use conventional commit messages (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`)
- Don't bump version numbers — maintainers handle releases

## Architecture

See [CLAUDE.md](CLAUDE.md) for project structure, key conventions, and file paths.

## Questions?

Open an issue or start a discussion on GitHub.
