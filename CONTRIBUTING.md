# Contributing

Thanks for helping improve Warp Storage Engine.

## Workflow

1. Open an issue or discussion before large changes so maintainers can align on direction.
2. Fork and branch from `main`, keep commits focused and messages descriptive.
3. Run tests locally (`make test` or `go test ./...`; for `pkg/vector` run `make test-vector` or `go test -tags vector ./pkg/vector/...`).
4. Open a pull request and describe behavior changes and risk.

## Releases

User-facing release notes belong on **GitHub Releases**. A root `CHANGELOG.md` may be produced locally by release tooling for packaging (for example GoReleaser) and can be gitignored; do not rely on it as the canonical history unless the maintainers change that policy.

## License

By contributing, you agree your contributions are licensed under the same terms as this repository: **Business Source License 1.1** ([MariaDB BSL 1.1](https://mariadb.com/bsl11)), with the parameters stated in [LICENSE](LICENSE) (Additional Use Grant, Change Date, and Change License). Do not submit third-party code unless you have the right to license it under those terms.

If you need an exception or commercial arrangement, contact the repository maintainers using the channels listed on the project homepage.
