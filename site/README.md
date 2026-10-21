# querdian.dev

The landing page and documentation, built with [Hugo](https://gohugo.io) and deployed to GitHub Pages by
`.github/workflows/pages.yml`.

* `layouts/index.html`, `assets/css/main.css` and `assets/js/pass.js` are the landing page.
* The documentation is the repository's `docs/*.md`: `build-content.sh` copies the pages listed in
  `data/nav.yaml` into `content/docs/` (generated, git-ignored) with front matter, and the link render hook
  maps links between pages to site paths. Add a page to the site by adding it to `data/nav.yaml`.
* The rewrites on the landing page are the output of the real enforcers for the quickstart policies. If the
  enforcers change how they rewrite, update `assets/js/pass.js`.
* Search is client-side: Hugo writes `search-index.json` (the plain text of every docs page, from
  `layouts/index.searchindex.json`) and `assets/js/search.js` scores it in the browser; there is no external
  service or dependency. Press `/` to open it.
* Fonts (Archivo, JetBrains Mono) are self-hosted under `static/fonts` and licensed under the SIL OFL 1.1.

```sh
make site          # generate docs pages and build into site/public
make site-serve    # live preview at http://localhost:1313
```

`make site` and the workflow pin the Hugo version in two places (`Makefile` and `pages.yml`); change both.

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
