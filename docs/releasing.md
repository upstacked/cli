# Releasing

Releases are cut by pushing a tag. GoReleaser builds six platform archives
(linux/darwin/windows × amd64/arm64), publishes a GitHub Release, and updates
the Homebrew cask in `upstacked/homebrew-tools`.

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Do **not** also run `goreleaser release` locally for the same tag: both the
workflow and the local run will try to upload the same assets, and the second
one fails with `already_exists`.

To rehearse without publishing anything:

```sh
goreleaser release --snapshot --clean --skip=publish
```

A rehearsal cannot catch everything. GoReleaser validates the cask's `token`
field only during a real publish, so neither `goreleaser check` nor a snapshot
will tell you it is malformed. Releasing from a throwaway tag is the only way
to exercise that path.

Running a real release locally without a tap token needs `--skip=homebrew`;
otherwise the cask step fails on the missing environment variable.

## Homebrew tap credentials

The workflow's `GITHUB_TOKEN` is scoped to `upstacked/cli`, so it cannot push
the formula to `upstacked/homebrew-tools`. A GitHub App supplies a short-lived
token scoped to the tap, with no personal credential stored in a secret.

Without those secrets the release still publishes binaries; only the cask
update is skipped, and the run logs a warning.

### One-time setup

1. Create the App at
   <https://github.com/organizations/upstacked/settings/apps/new>
   - **Name**: `upstacked-tap-publisher`
   - **Homepage**: `https://github.com/upstacked/cli`
   - Uncheck **Webhook → Active**
   - **Repository permissions → Contents**: *Read and write*
     (this is the only permission needed)
2. **Create GitHub App**, then note the **App ID**.
3. **Generate a private key** and download the `.pem`.
4. **Install App** → *Only select repositories* → `upstacked/homebrew-tools`.
5. Add the two secrets to `upstacked/cli`:

   ```sh
   gh secret set TAP_APP_ID --repo upstacked/cli --body "<app id>"
   gh secret set TAP_APP_PRIVATE_KEY --repo upstacked/cli < path/to/key.pem
   ```

6. Verify with a patch tag; the run should show the cask being pushed.

## Verifying an install

```sh
brew update
brew install upstacked/tools/cli   # or: brew upgrade --cask upstacked/tools/cli
ups version
```
