# querdian.dev

The landing page and documentation, built with [Hugo](https://gohugo.io) and deployed to GitHub Pages by
`.github/workflows/pages.yml`.

* `layouts/index.html`, `assets/css/main.css` and `assets/js/pass.js` are the landing page.
* The documentation is the repository's `docs/*.md`: `build-content.sh` copies the pages listed in
  `data/nav.yaml` into `content/docs/` (generated, git-ignored) with front matter, and the link render hook
  maps links between pages to site paths. Add a page to the site by adding it to `data/nav.yaml`.
  `docs-overview.md` is the overview page of the documentation.
* The documentation has versions, see [Documentation versions](#documentation-versions).
* The rewrites on the landing page are the output of the real enforcers for the quickstart policies. If the
  enforcers change how they rewrite, update `assets/js/pass.js`.
* Search is client-side: Hugo writes one `search-index.json` for each documentation version (the plain
  text of every docs page of that version, from `layouts/_default/list.searchindex.json`) and
  `assets/js/search.js` scores the index of the version of the current page in the browser; there is no
  external service or dependency. Press `/` to open it.
* Fonts (Archivo, JetBrains Mono) are self-hosted under `static/fonts` and licensed under the SIL OFL 1.1.

```sh
make site          # generate docs pages, build into site/public and check the internal links
make site-serve    # live preview at http://localhost:1313
```

The Hugo version is pinned in the `Makefile` only; the workflow runs `make site`.

## Documentation versions

`build-content.sh` writes one documentation tree for each version. It finds the versions itself:

| Version | Source | Address |
|---|---|---|
| Latest release | the newest release tag `vMAJOR.MINOR.PATCH` | `/docs/` |
| Older release lines | the latest patch tag of each older `vMAJOR.MINOR` line | `/docs/<tag>/` |
| `main` | the working tree | `/docs/main/` |

* Each version uses its own `docs/`, `site/data/nav.yaml` and overview page, read from its tag with
  git. The layouts, styles and scripts are always those of the working tree. Thus a change to the
  layouts must also work with the content of the older tags; the pull request build shows this.
* Pre-release tags and earlier patches of a release line are not published. Each page of a published
  version also answers at the address of the earlier patches of its line (`/docs/v0.1.1/...`), and each
  page of the latest release at the address with its tag (`/docs/v0.1.2/...`): these addresses redirect
  to the page (Hugo aliases). A page that a later patch removed has no redirect.
* The versions come from the tags, not from the GitHub releases. A tag whose release workflow failed
  still becomes a documentation version with the next build of the website.
* A tag does not change. A correction to the documentation of a release is published with the next patch
  release of its line.
* The generated `data/versions.yaml` (git-ignored) lists the versions with their navigation.
  `layouts/partials/docs-version.html` gives the version of a page to the layouts, and
  `layouts/partials/docs-versions.html` is the version selector.
* The landing page, the header and the footer link into the latest release. A page that is only on
  `main` is a broken link there until its release. `hack/sitecheck` finds such links and stops
  `make site`. Link to `/docs/main/...` if a page of `main` is necessary.
* Pages of other versions name the same page of the latest release as their canonical address for search
  engines.
* Without release tags (a clone without tags, a source archive) the working tree is the only version,
  at `/docs/`. The workflow clones the full history and fails if `/docs/main/` is absent. A git error
  in a repository stops `build-content.sh`.

## Publishing

1. Repository settings, Pages: source "GitHub Actions", custom domain `querdian.dev`, enforce HTTPS once the
   certificate is issued.
2. DNS for the apex domain (GitHub Pages values): `A` records `185.199.108.153`, `185.199.109.153`,
   `185.199.110.153`, `185.199.111.153` and `AAAA` records `2606:50c0:8000::153`, `2606:50c0:8001::153`,
   `2606:50c0:8002::153`, `2606:50c0:8003::153`. Optionally a `CNAME` for `www` pointing at
   `querdian-dev.github.io`.
3. Verify the domain for the organisation (Settings, Pages) to prevent takeover.
4. Set `repoPublic = true` in `hugo.toml` when the repository is public: this turns on the GitHub links in the
   header, footer and docs.
