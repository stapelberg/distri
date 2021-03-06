package checkupstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distr1/distri"
	"github.com/protocolbuffers/txtpbfmt/ast"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

func checkDebian(debianPackagesURL, source string) (remoteSource, remoteHash, remoteVersion string, _ error) {
	ctx, canc := context.WithTimeout(context.Background(), 5*time.Second)
	defer canc()
	req, err := http.NewRequestWithContext(ctx, "GET", debianPackagesURL, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return "", "", "", fmt.Errorf("%v: HTTP %v", debianPackagesURL, resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(string(b), "\n")
	sourceUrl, err := url.Parse(source)
	if err != nil {
		return "", "", "", err
	}
	sourceUrl.Path = path.Dir(sourceUrl.Path)
	var filename string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "Filename: "):
			filename = strings.TrimPrefix(line, "Filename: ")
			continue

		case strings.HasPrefix(line, "Version: "):
			remoteVersion = strings.TrimPrefix(line, "Version: ")
			continue

		case strings.HasPrefix(line, "SHA256: "):
			remoteHash = strings.TrimPrefix(line, "SHA256: ")
			continue

		case strings.TrimSpace(line) == "":
			// package stanza done

		default:
			continue
		}

		if !strings.HasSuffix(sourceUrl.Path, path.Dir(filename)) {
			continue
		}
		remoteBase := path.Base(filename)
		sourceUrl.Path = path.Join(sourceUrl.Path, remoteBase)
		return sourceUrl.String(), remoteHash, remoteVersion, nil
	}
	return "", "", "", fmt.Errorf("package not found in Debian Packages file")
}

func maybeV(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

func checkHeuristic(upstreamVersion, source, releasesURL string) (remoteSource, remoteHash, remoteVersion string, _ error) {
	u, _ := url.Parse(releasesURL)
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("unexpected HTTP response: got %v, want 200 OK", resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	// TODO: add special casing for parsing apache directory index?
	base := path.Base(source)
	log.Printf("base: %v", base)
	idx := strings.Index(base, upstreamVersion)
	if idx == -1 {
		return "", "", "", fmt.Errorf("upstreamVersion %q not found in base %q", upstreamVersion, base)
	}
	pattern := regexp.QuoteMeta(base[:idx]) + `([0-9vp.-]*)` + regexp.QuoteMeta(base[idx+len(upstreamVersion):])
	if pattern == path.Base(source) {
		return "", "", "", fmt.Errorf("could not derive regexp pattern, specify manually")
	}
	log.Printf("pattern: %v", pattern)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", "", "", err
	}
	log.Printf("re: %v", re)
	matches := re.FindAllStringSubmatch(string(b), -1)
	log.Printf("matches: %v", matches)
	versions := func() []string {
		v := make(map[string]bool)
		for _, m := range matches {
			if m[1] == "latest" {
				continue // skip common latest symlink
			}
			v[m[1]] = true
		}
		result := make([]string, 0, len(v))
		for version := range v {
			result = append(result, version)
		}
		valid := true
		for _, r := range result {
			if !semver.IsValid(maybeV(r)) {
				valid = false
				break
			}
		}
		if !valid {
			// Prefer a string sort when the versions aren’t semver, it’s better
			// than semver.Compare.
			sort.Sort(sort.Reverse(sort.StringSlice(result)))
		} else {
			sort.Slice(result, func(i, j int) bool {
				v, w := result[i], result[j]
				v, w = maybeV(v), maybeV(w)
				return semver.Compare(v, w) >= 0 // reverse
			})
		}
		return result
	}()
	if len(versions) == 0 {
		return "", "", "", fmt.Errorf("not yet implemented")
	}
	u.Path = path.Join(u.Path, strings.Replace(path.Base(source), upstreamVersion, versions[0], 1))
	return u.String(), "TODO", versions[0], nil
}

// e.g. checkGoMod(github.com/lpar/gzipped@v1.1.0)
func checkGoMod(v string) (remoteSource, remoteHash, remoteVersion string, _ error) {
	mod := v
	if idx := strings.Index(mod, "@"); idx > -1 {
		mod = mod[:idx]
	}
	// https://github.com/golang/go/blob/master/src/cmd/go/internal/modfetch/proxy.go
	modEsc, err := module.EscapePath(mod)
	if err != nil {
		return "", "", "", err
	}
	u := "https://proxy.golang.org/" + modEsc + "/@latest"
	resp, err := http.Get(u)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("%s: unexpected HTTP status: got %v, want OK", u, resp.Status)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	var version struct{ Version string }
	if err := json.Unmarshal(b, &version); err != nil {
		return "", "", "", err
	}

	remoteVersion = version.Version
	remoteSource = mod + "@" + remoteVersion
	return remoteSource, remoteHash, remoteVersion, nil
}

var projectRe = regexp.MustCompile(`^/project/([^/]+)/`)

func Check(build []*ast.Node) (source, hash, version string, _ error) {
	stringVal := func(path ...string) (string, error) {
		nodes := ast.GetFromPath(build, path)
		if got, want := len(nodes), 1; got != want {
			return "", fmt.Errorf("malformed build file: got %d version keys, want %d", got, want)
		}
		values := nodes[0].Values
		if got, want := len(values), 1; got != want {
			return "", fmt.Errorf("malformed build file: got %d Values, want %d", got, want)
		}
		return strconv.Unquote(values[0].Value)
	}
	source, err := stringVal("source")
	if err != nil {
		return "", "", "", err
	}

	if strings.HasSuffix(source, ".deb") {
		u, err := stringVal("pull", "debian_packages")
		if err != nil {
			return "", "", "", err
		}
		return checkDebian(u, source)
	}

	if strings.HasPrefix(source, "distri+gomod://") {
		return checkGoMod(strings.TrimPrefix(source, "distri+gomod://"))
	}

	version, err = stringVal("version")
	if err != nil {
		return "", "", "", err
	}

	pv := distri.ParseVersion(version)
	// fall back: see if we can obtain an index page
	releases, err := stringVal("pull", "releases_url")
	if err != nil {
		u, err := url.Parse(source)
		if err != nil {
			return "", "", "", err
		}
		if u.Host == "github.com" && strings.Contains(u.Path, "/releases/") {
			u.Path = u.Path[:strings.Index(u.Path, "/releases/")+len("/releases/")]
		} else if u.Host == "github.com" && strings.Contains(u.Path, "/archive/") {
			u.Path = u.Path[:strings.Index(u.Path, "/archive/")] + "/releases/"
		} else if u.Host == "downloads.sourceforge.net" && strings.HasPrefix(u.Path, "/project/") {
			project := projectRe.FindStringSubmatch(u.Path)
			if got, want := len(project), 2; got != want {
				return "", "", "", fmt.Errorf("downloads.sourceforge.net: got %d matches, want %d", got, want)
			}
			u.Path = "/projects/" + project[1] + "/files/"
			u.Host = "sourceforge.net"
			// u e.g. https://sourceforge.net/projects/bzip2/files/
		} else if u.Host == "launchpad.net" {
			// e.g. https://launchpad.net/lightdm-gtk-greeter/2.0/2.0.6/+download
			u.Path = strings.TrimPrefix(u.Path, "/")
			if idx := strings.Index(u.Path, "/"); idx > -1 {
				u.Path = u.Path[:idx]
			}
			// e.g. https://launchpad.net/lightdm-gtk-greeter/
		} else {
			u.Path = path.Dir(u.Path)
		}
		releases = u.String()
	}

	log.Printf("releases: %s", releases)
	return checkHeuristic(pv.Upstream, source, releases)
}
