// Command sitecheck checks the built website (site/public): every internal link of every HTML
// page must point to a file of the site. `make site` runs it after Hugo, so a broken link fails
// the build and the deployment.
//
//	go run ./hack/sitecheck -dir site/public -base https://querdian.dev/
//
// The site holds one documentation tree per version (site/build-content.sh), while the landing
// page, the header and the footer come from main and link into the latest release. A page that
// exists on main only is therefore a broken link until it is released; this check finds it.
//
// Internal links are the href, src and data-index attributes (data-index is the address of the
// search index) and the targets of redirect pages (meta refresh) that are relative, start with
// "/" or start with the address of the site. Links to other hosts are not fetched and fragments
// are not checked. Other places of an address (srcset, form actions, url() in style sheets) are
// not read; the site does not use them for internal files other than its fonts.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

func main() {
	dir := flag.String("dir", "site/public", "directory of the built site")
	base := flag.String("base", "", "address of the site, for example https://querdian.dev/; links that start with it are internal")
	flag.Parse()

	broken, pages, err := check(os.DirFS(*dir), *base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sitecheck:", err)
		os.Exit(1)
	}
	if pages == 0 {
		fmt.Fprintf(os.Stderr, "sitecheck: no HTML pages in %s\n", *dir)
		os.Exit(1)
	}
	for _, b := range broken {
		fmt.Fprintln(os.Stderr, b)
	}
	if len(broken) > 0 {
		fmt.Fprintf(os.Stderr, "sitecheck: %d broken internal links\n", len(broken))
		os.Exit(1)
	}
	fmt.Printf("sitecheck: %d pages, all internal links resolve\n", pages)
}

// check returns the broken internal links of the HTML pages in fsys as sorted "page: link" lines,
// and the number of pages it read.
func check(fsys fs.FS, base string) (broken []string, pages int, err error) {
	err = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(name, ".html") {
			return nil
		}
		f, err := fsys.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		doc, err := html.Parse(f)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		pages++
		for _, link := range links(doc) {
			target, internal := resolve(name, link, base)
			if internal && !exists(fsys, target) {
				broken = append(broken, name+": "+link)
			}
		}
		return nil
	})
	sort.Strings(broken)
	return broken, pages, err
}

// links returns the link targets of a page: href, src and data-index attributes and the address
// of a meta refresh redirect.
func links(n *html.Node) []string {
	var out []string
	if n.Type == html.ElementNode {
		refresh := false
		for _, a := range n.Attr {
			switch {
			case a.Key == "href" || a.Key == "src" || a.Key == "data-index":
				out = append(out, a.Val)
			case n.Data == "meta" && a.Key == "http-equiv" && strings.EqualFold(a.Val, "refresh"):
				refresh = true
			}
		}
		if refresh {
			for _, a := range n.Attr {
				if a.Key != "content" {
					continue
				}
				// "0; url=https://querdian.dev/docs/quickstart/"
				if _, target, ok := strings.Cut(a.Val, "="); ok {
					out = append(out, strings.TrimSpace(target))
				}
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, links(c)...)
	}
	return out
}

// resolve maps a link on the page (a path below the site root) to the file that serves it.
// internal is false for links to other hosts, other schemes and for fragments of the same page.
func resolve(page, link, base string) (target string, internal bool) {
	link = strings.TrimSpace(link)
	if base != "" && strings.HasPrefix(link, base) {
		link = "/" + strings.TrimPrefix(link, base)
	}
	u, err := url.Parse(link)
	if err != nil {
		// A link that does not parse cannot be served: report it.
		return "", true
	}
	if u.Scheme != "" || u.Host != "" || u.Path == "" {
		return "", false
	}
	p := u.Path
	if !strings.HasPrefix(p, "/") {
		p = path.Join("/", path.Dir(page), p)
		if strings.HasSuffix(u.Path, "/") {
			p += "/"
		}
	}
	dir := strings.HasSuffix(p, "/")
	target = strings.TrimPrefix(path.Clean(p), "/")
	switch {
	case target == "":
		target = "index.html"
	case dir:
		target += "/index.html"
	}
	return target, true
}

// exists reports whether the site serves the file: the file itself or, for a directory, its
// index.html.
func exists(fsys fs.FS, target string) bool {
	if target == "" || !fs.ValidPath(target) {
		return false
	}
	info, err := fs.Stat(fsys, target)
	if err != nil {
		return false
	}
	if info.IsDir() {
		_, err = fs.Stat(fsys, path.Join(target, "index.html"))
		return err == nil
	}
	return true
}
