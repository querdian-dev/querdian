# Releasing

The exact procedure for cutting a Querdian release. It is written for the maintainer and for
task T5.7 (release 0.1.0). **Nothing here has been run**: no tag exists, the release workflow
([.github/workflows/release.yml](../.github/workflows/release.yml)) has never executed, and
several statements below (marked "unverified") describe what the configuration should do. The
dry-run with a release candidate exists precisely to turn them into facts.

What a tag push `v*` does, by reading the workflow and [.goreleaser.yaml](../.goreleaser.yaml):

1. Rejects tags that are not `vMAJOR.MINOR.PATCH[-pre]`, and versions without CHANGELOG entries
   (see [Artifact Hub](#artifact-hub)).
2. GoReleaser builds `linux/amd64` and `linux/arm64` binaries, `tar.gz` archives, a SHA-256
   checksums file and an SPDX SBOM per archive (syft), and creates the GitHub release (a tag with a
   pre-release part is marked as pre-release; `latest` is only tagged for non-pre-release versions).
3. GoReleaser builds the multi-arch image `ghcr.io/querdian-dev/querdian:<version>` from the already
   built binaries (image SBOM enabled) and signs the manifest with cosign (keyless).
4. cosign signs the checksums file (a Sigstore bundle `.sigstore.json` uploaded to the release).
5. GitHub build-provenance attestations are created for the archives (via the checksums file) and
   for the image digest (pushed to the registry).
6. The `chart` job writes the per-version Artifact Hub annotations into `Chart.yaml`
   (`hack/chartmeta`: the changelog of the version from CHANGELOG.md, `prerelease`,
   `containsSecurityUpdates`, `helm.sh/images`), packages `charts/querdian` with `--version` and
   `--app-version` set to the tag without `v`, pushes it to `oci://ghcr.io/querdian-dev/charts` and
   signs the chart digest with cosign (keyless). The chart is **not** covered by a GitHub
   attestation.
7. The `chart` job pushes [charts/artifacthub-repo.yml](../charts/artifacthub-repo.yml) to
   `ghcr.io/querdian-dev/charts/querdian:artifacthub.io` with oras (see
   [Artifact Hub](#artifact-hub)).

## 1. One-time GitHub settings

Do these once, before the first tag. Items marked HUMAN in
[openssf-best-practices.md](openssf-best-practices.md) (H1 to H11) overlap with this list.

### Repository and organization

* Repository public, Issues enabled, private vulnerability reporting enabled, secret scanning with
  push protection and Dependabot alerts enabled (H1, H2, H4).
* Renovate app installed for the organization (H11).
* Settings > Actions > General: workflow permissions "Read repository contents" by default (the
  workflows declare their own `permissions:`), and **"Allow GitHub Actions to create and approve pull
  requests" off**.

### Actions allowlist

The organization policy (T0.0) allows GitHub-owned and verified-creator actions only. Whether a
given creator is "verified" is shown on the Marketplace page; if one of the following is not, add it
explicitly to the allowlist. The full list used by the workflows today (all pinned by commit SHA
in the files; Renovate bumps them):

| Action | Used in |
|---|---|
| `actions/checkout` | all workflows |
| `actions/setup-go` | ci, codeql, release |
| `actions/upload-artifact` | scorecard |
| `actions/attest-build-provenance` | release |
| `github/codeql-action/init`, `analyze`, `upload-sarif` | codeql, scorecard |
| `docker/setup-qemu-action`, `docker/setup-buildx-action`, `docker/login-action` | release |
| `sigstore/cosign-installer` | release (twice) |
| `anchore/sbom-action/download-syft` | release |
| `goreleaser/goreleaser-action` | release |
| `azure/setup-helm` | release |
| `ossf/scorecard-action` | scorecard |

A suitable allowlist entry list (SHA pins are matched by owner/repo):

```
actions/*, github/codeql-action/*, docker/setup-qemu-action@*, docker/setup-buildx-action@*,
docker/login-action@*, sigstore/cosign-installer@*, anchore/sbom-action/download-syft@*,
goreleaser/goreleaser-action@*, azure/setup-helm@*, ossf/scorecard-action@*
```

Compare against `grep -h 'uses:' .github/workflows/*.yml | sort -u` before the release; the list
above was read from the files at the time of writing.

### Ruleset for `main`

Settings > Rules > Rulesets, target `main`:

* Require a pull request before merging. With one maintainer do not require approvals (that would
  block every merge); GOVERNANCE.md's second-reviewer rule cannot be enforced until there is a
  second maintainer (H10).
* Block force pushes, block deletions.
* Require status checks to pass. These are the job names in the workflows (they only become
  selectable after they have run once on a PR):

  | Check | Workflow |
  |---|---|
  | `lint` | ci.yml |
  | `test-race` | ci.yml |
  | `envtest` | ci.yml |
  | `fuzz` | ci.yml |
  | `govulncheck` | ci.yml |
  | `licenses` | ci.yml |
  | `generate` | ci.yml |
  | `dco` | dco.yml (PRs only) |
  | `codeql (go)` | codeql.yml |

  `scorecard` runs on `main`, a schedule and branch-protection changes only; do not require it.
  `ci.yml` also has a `chart` job (CRD sync check, `helm lint`, chart tests) and an `e2e` job (the
  differential suites against real Prometheus, Loki and Tempo containers). Both are new and have never
  run on GitHub: do not require them until they have been green for a while, then add them to the list
  above. There is no CI job for `make release-check`; it is run locally in the pre-flight below.
* Add a tag ruleset for `v*`: restrict creation to maintainers, block deletion and updates (tags
  are the root of the signing identity; see rollback below).

### Packages (ghcr.io)

The first push creates the packages `querdian` and `charts/querdian` under the organization. New
packages default to private. After the first push (the release candidate):

* Package settings > "Manage Actions access": make sure the repository has write access (it is
  linked automatically when the image carries the `org.opencontainers.image.source` label, which
  the GoReleaser configuration sets; unverified for the chart package, whose OCI annotations come
  from `Chart.yaml` `sources`).
* Package settings > Change visibility > **Public**, for both packages. Otherwise `helm pull` and
  `docker pull` from the quickstart fail for users.

### Artifact Hub

The chart is listed on [Artifact Hub](https://artifacthub.io) straight from the OCI registry; there
is no separate Helm repository. What Artifact Hub shows comes from three places:

* Static annotations in [Chart.yaml](../charts/querdian/Chart.yaml) (category, license, links,
  CRDs and CRD examples). `charts/querdian/artifacthub_test.go` checks them against `crds/` and the
  API types.
* Per-version annotations written by the release job with `go run ./hack/chartmeta -version
  <version>`. `artifacthub.io/changes` is the **changelog** Artifact Hub displays: the entries under
  Added, Changed, Deprecated, Removed, Fixed and Security of the `## [<version>]` section of
  [CHANGELOG.md](../CHANGELOG.md) (one change per top-level bullet; `####` sub-headings and other
  `###` sections such as "Known limitations" are left out). A pre-release uses the section of its
  release version or, failing that, `[Unreleased]`. **A release without entries in its own
  section fails the release workflow** in its first step, before anything is published; check with
  `go run ./hack/chartmeta -version <version> -out -` in the pre-flight. `containsSecurityUpdates` is
  `true` when the section has a Security entry, `prerelease` when the version has a pre-release
  part. Artifact Hub does not re-index an OCI chart version, so what a version was pushed with
  stays.
* The repository metadata [charts/artifacthub-repo.yml](../charts/artifacthub-repo.yml), pushed by
  the release job to the tag `artifacthub.io` of the chart repository: owners (ownership claim),
  `repositoryID` (verified publisher) and ignored versions (release candidates, `-rc.`).

One-time setup, after the first chart version is public on ghcr.io (Artifact Hub cannot read
private packages):

1. Sign in to <https://artifacthub.io> with the account whose email is listed under `owners` in
   `charts/artifacthub-repo.yml`. Optionally create an organization `querdian-dev` and add the
   repository there.
2. Control panel > Repositories > Add: kind **Helm charts**, name `querdian`, URL
   `oci://ghcr.io/querdian-dev/charts/querdian`.
3. Copy the repository ID from the repository card into `repositoryID` in
   `charts/artifacthub-repo.yml` (pull request). The metadata is pushed with the next release;
   to apply it earlier, push it by hand after `docker login ghcr.io`:

   ```sh
   cd charts && go run oras.land/oras/cmd/oras@v1.3.4 push \
     ghcr.io/querdian-dev/charts/querdian:artifacthub.io \
     --config /dev/null:application/vnd.cncf.artifacthub.config.v1+yaml \
     artifacthub-repo.yml:application/vnd.cncf.artifacthub.repository-metadata.layer.v1.yaml
   ```

   Artifact Hub sets the verified-publisher badge the next time it processes the repository.

The `artifacthub.io` tag shows up among the versions of the `charts/querdian` package on ghcr.io.
It is an OCI artifact, not a chart: `helm` and Artifact Hub ignore it as a chart version.

## 2. Pre-flight checklist

On a clean checkout of the commit to be tagged (`main`, up to date, CI green):

```sh
git status --short                       # must be empty
make generate manifests && git diff --exit-code    # no generated drift
make lint test-race envtest e2e licenses chart-lint release-check
```

* `e2e` pulls Prometheus, Loki and Tempo containers and needs a working container runtime; it takes
  a while (the target timeout is 20 minutes).
* `release-check` runs `goreleaser check` through the module proxy (needs network).
* `chart-lint` needs `helm`.
* `go run ./hack/chartmeta -version $VERSION -out -` must succeed and show the expected
  `artifacthub.io/changes`; it fails if the CHANGELOG section of the version has no entries.
* Run the quickstart against the commit: `hack/quickstart/up.sh && hack/quickstart/verify.sh &&
  hack/quickstart/down.sh` (needs kind; builds the image from source).
* Optionally `make fuzz FUZZTIME=2m` and `go run golang.org/x/vuln/cmd/govulncheck@<pinned> ./...`
  (the pin is in `ci.yml`).
* Confirm that the Scorecard, CodeQL and CI runs on `main` are green and that there are no open
  security alerts.

Documents to update in the release commit (a pull request, like any change):

* [CHANGELOG.md](../CHANGELOG.md): move the entries from `[Unreleased]` into the new version, set
  the date, fill the compare links; keep an empty `[Unreleased]`. The entries of the version also
  become the Artifact Hub changelog (see [Artifact Hub](#artifact-hub)), so keep them readable as
  one-paragraph changes.
* [release-notes-0.1.0.md](release-notes-0.1.0.md): re-read every limitation against
  [limitations.md](limitations.md); strike "draft".
* [README.md](../README.md): the status paragraph, and uncomment the badges only if H5 and H8 are done.
* [SECURITY.md](../SECURITY.md): "Supported versions" still says "pre-alpha; only the latest `main`
  is supported". Replace it with the supported release line (for example "the latest 0.x minor").
* [TASKS.md](../TASKS.md): tick T5.2 (only after the dry-run proved the multi-arch image, signatures
  and chart publication) and T5.7 (only when every acceptance item below is done).
* `Chart.yaml` stays at `0.0.0-dev`; the pipeline sets version and appVersion.
* [docs/quickstart.md](quickstart.md) and the chart README still say "no published image yet";
  adjust once a release exists.

## 3. Dry-run with a release candidate

Strongly recommended before `v0.1.0`: the pipeline has never run, and a bad `v0.1.0` cannot be
cleanly retracted (see rollback).

```sh
git switch main && git pull --ff-only
git tag -s v0.0.0-rc.1 -m "Querdian v0.0.0-rc.1 (release pipeline dry-run)"   # or -a if you do not sign tags
git push origin v0.0.0-rc.1
```

(`v0.1.0-rc.1` is equally valid and reads better on the releases page; either is a pre-release and
does not move `latest`. The suggested version string `0.0.0-rc.1` keeps the dry-run visibly outside
the 0.1.x line.)

Watch the `Release` workflow, then perform **every verification of section 5** against
`0.0.0-rc.1`. Record what failed in the pull request that fixes it. Typical first-run problems
(all unverified guesses): package visibility or write access for ghcr, an action blocked by the
allowlist, cosign identity regexp not matching, the chart push digest parsing in the workflow.
Fix, delete the rc release and tag (section 6), tag `v0.0.0-rc.2`.

## 4. The release

```sh
git switch main && git pull --ff-only
git log -1 --oneline                      # the reviewed release commit, CI green on it
git tag -s v0.1.0 -m "Querdian v0.1.0"
git push origin v0.1.0
```

Then:

1. Wait for the workflow (`release` and `publish chart` jobs, about 45 minutes at most).
2. Edit the release body. GoReleaser generates a changelog from commit messages and appends a
   "Verify" footer. To use the curated notes, keep the footer and paste the content of
   [release-notes-0.1.0.md](release-notes-0.1.0.md) above the generated part
   (`gh release edit v0.1.0 --notes-file <file>` replaces the whole body including the footer, so
   copy the footer in).
3. Verify (section 5).
4. Set both ghcr packages to public if not done after the rc.
5. Announce only after verification passed.

## 5. Post-release verification

Replace `VERSION` (without `v`). Needs `cosign` v3+ (bundle format), `gh`, `helm`, `docker`. The certificate identity
is the release workflow at a tag ref, exactly as in the GoReleaser footer.

```sh
VERSION=0.1.0
IDENTITY='^https://github.com/querdian-dev/querdian/\.github/workflows/release\.yml@refs/tags/'
ISSUER=https://token.actions.githubusercontent.com
```

**Image signature and provenance**

```sh
cosign verify ghcr.io/querdian-dev/querdian:$VERSION \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"

gh attestation verify oci://ghcr.io/querdian-dev/querdian:$VERSION --repo querdian-dev/querdian
docker buildx imagetools inspect ghcr.io/querdian-dev/querdian:$VERSION   # linux/amd64 and linux/arm64
```

Pin the digest from `imagetools inspect` (or `docker manifest inspect`) in the values you publish.

**Release archives**

```sh
mkdir verify-$VERSION && cd verify-$VERSION
gh release download v$VERSION --repo querdian-dev/querdian
cosign verify-blob querdian_${VERSION}_checksums.txt \
  --bundle querdian_${VERSION}_checksums.txt.sigstore.json \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
sha256sum --check --ignore-missing querdian_${VERSION}_checksums.txt
gh attestation verify querdian_${VERSION}_linux_amd64.tar.gz --repo querdian-dev/querdian
tar -xzf querdian_${VERSION}_linux_amd64.tar.gz && ./querdian --version   # prints "querdian $VERSION" (host must be linux/amd64)
```

The SBOMs (`*.sbom.spdx.json`) are release assets; open one and check that it lists the Go modules.

**Helm chart**

```sh
helm pull oci://ghcr.io/querdian-dev/charts/querdian --version $VERSION
tar -tzf querdian-$VERSION.tgz | head          # contains crds/ and values.schema.json
helm show chart oci://ghcr.io/querdian-dev/charts/querdian --version $VERSION | grep -E '^(version|appVersion):'
cosign verify ghcr.io/querdian-dev/charts/querdian:$VERSION \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
```

Both `version` and `appVersion` must equal `$VERSION`, and the chart must pull the image of the same
version by default. `helm show chart` must also list `artifacthub.io/changes`, `artifacthub.io/prerelease`
and `helm.sh/images` with the image of `$VERSION`.

**Artifact Hub** (after the one-time setup in [Artifact Hub](#artifact-hub))

```sh
go run oras.land/oras/cmd/oras@v1.3.4 manifest fetch ghcr.io/querdian-dev/charts/querdian:artifacthub.io
```

Then open <https://artifacthub.io/packages/helm/querdian/querdian> (the path depends on the
repository name chosen there) once Artifact Hub has processed the repository (it polls, which can
take up to an hour): the version, the changelog modal, the CRDs and, after the scan, the image
security report must be there.

**Quickstart against the released artefacts**

`up.sh` builds the image from the checkout by default and installs the chart from the checkout. Use
the released image instead, then swap in the released chart:

```sh
git switch --detach v$VERSION                      # scripts and manifests of the tag
docker pull ghcr.io/querdian-dev/querdian:$VERSION
SKIP_BUILD=1 IMAGE_TAG=$VERSION hack/quickstart/up.sh
helm upgrade querdian oci://ghcr.io/querdian-dev/charts/querdian --version $VERSION \
  -n querdian --reuse-values --wait
hack/quickstart/verify.sh                          # all assertions must pass
hack/quickstart/down.sh
```

`--reuse-values` keeps the quickstart configuration and the image tag (`pullPolicy: Never`, the image
was loaded into kind). This checks the published image and chart together; the quickstart scripts
themselves are the ones of the tag. Not verified: that the stack works with the image pulled by the
cluster from ghcr.io (the quickstart loads it), so also run once with a values override of
`image.pullPolicy=IfNotPresent` or on a real cluster.

## 6. Rollback

Signatures are written to the public Rekor log and cannot be withdrawn; tags and versions are
public. The rule is therefore **never move or reuse a tag or a chart version**.

* **Release candidate that failed:** delete the GitHub release and tag
  (`gh release delete v0.0.0-rc.1 --cleanup-tag --yes`), delete the image tag and chart version in
  the package settings (or `gh api -X DELETE /orgs/querdian-dev/packages/container/<name>/versions/<id>`),
  and use the next rc number. A tag that was never published can be deleted locally and on `origin`.
* **A bad `v0.1.0`:** do not delete and re-push. Mark the GitHub release as a pre-release or edit the
  notes to say it is withdrawn and why, publish `v0.1.1` with the fix, and add a "Yanked" note to
  CHANGELOG.md. If users may already have pulled a dangerous artefact, say so in a GitHub security
  advisory (see [SECURITY.md](../SECURITY.md)).
* **Re-running the workflow on an existing tag** (`workflow_dispatch` is not configured; re-running
  the failed run in the UI is possible): GoReleaser runs with `--clean` and `release.mode: replace`,
  so the release assets and body are replaced. Pushing the chart with the same version again
  overwrites the OCI tag with a different digest. That is acceptable for a release that failed half
  way before anyone consumed it, never afterwards.
* **Chart-only failure** (`publish chart` job failed, release fine): fix the workflow in a pull
  request and re-run the failed job from the run for the same tag, or release `v0.1.1`.

## 7. Acceptance for T5.7

Tick T5.7 in [TASKS.md](../TASKS.md) when all of this is true:

* [ ] CHANGELOG and release notes are final.
* [ ] Tag `v0.1.0` exists and the release pipeline is green.
* [ ] Section 5 verification passed for image, archives and chart.
* [ ] The quickstart ran against the released artefacts and `verify.sh` passed.
* [ ] Both ghcr.io packages are public.
* [ ] The chart is listed on Artifact Hub with the 0.1.0 changelog.
