Regenerate `source.bundle` from the repository root with Git, using a fresh
scratch directory and the fixed identity/date below. The expected HEAD is
`38a7fa625016861399920bfd0fbea8b0ef053773` (`portableHead` in fixture_test.go).

```sh
(
fixture=$(mktemp -d)
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_ATTR_NOSYSTEM=1
export GIT_AUTHOR_NAME=Fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=Fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
export GIT_AUTHOR_DATE='1767225600 +0000' GIT_COMMITTER_DATE='1767225600 +0000'
git init --template= --object-format=sha1 -b main "$fixture"
printf 'source\n' > "$fixture/source.txt"
git -C "$fixture" add --force source.txt
git -C "$fixture" -c core.hooksPath=/dev/null commit -m 'fixture source'
git -C "$fixture" tag fixture-tag
test "$(git -C "$fixture" rev-parse HEAD)" = 38a7fa625016861399920bfd0fbea8b0ef053773
git -C "$fixture" bundle create "$PWD/internal/action/testdata/source.bundle" --all
rm -rf "$fixture"
)
```

The fixture contains one regular `source.txt` file with `source` plus newline,
one commit and the lightweight `fixture-tag`. It needs no remote or prerequisite.
