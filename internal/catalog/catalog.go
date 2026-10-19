// Package catalog loads app definitions (embedded YAML, overridable by a local dir)
// and reads app config files according to them.
package catalog

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	embedded "github.com/mathieuletyrant/arrglue/catalog"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

type App struct {
	Kind   string `yaml:"kind"`
	Family string `yaml:"family"`
	Detect struct {
		Images   []string `yaml:"images"`
		Binaries []string `yaml:"binaries"`
		Systemd  []string `yaml:"systemd"`
	} `yaml:"detect"`
	Links  []string       `yaml:"links"` // kinds this app connects to
	Ports  map[string]int `yaml:"ports"`
	Config struct {
		Paths    []string          `yaml:"paths"`
		Format   string            `yaml:"format"`   // xml | json | yaml | ini
		Fields   map[string]string `yaml:"fields"`   // name -> /path/in/tree
		Defaults map[string]string `yaml:"defaults"` // used when the config cannot be read
	} `yaml:"config"`
	// Secret: where to find the API key when it is not in the config file.
	Secret struct {
		Paths  []string `yaml:"paths"`  // app-side paths of a SQLite database
		SQLite string   `yaml:"sqlite"` // query returning the key in its first column
		Env    string   `yaml:"env"`    // container environment variable holding the key
	} `yaml:"secret"`
	Inject struct {
		Env map[string]string `yaml:"env"`
	} `yaml:"inject"`
	Health struct {
		Path string `yaml:"path"`
		Auth struct {
			Header string `yaml:"header"`
		} `yaml:"auth"`
		VersionField string `yaml:"version_field"`
		OK           []int  `yaml:"ok"` // accepted statuses, default [200]
	} `yaml:"health"`
	API struct {
		Versions    []string `yaml:"versions"`
		RootFolders string   `yaml:"root_folders"` // GET endpoint listing {path} objects
	} `yaml:"api"`
}

type Catalog struct{ Apps map[string]*App }

// Load reads the embedded catalog, then overrides/adds from dir (if non-empty).
func Load(dir string) (*Catalog, error) {
	c := &Catalog{Apps: map[string]*App{}}
	if err := c.loadFS(embedded.FS, "."); err != nil {
		return nil, err
	}
	if dir != "" {
		if err := c.loadFS(os.DirFS(dir), "."); err != nil {
			return nil, fmt.Errorf("catalog override %s: %w", dir, err)
		}
	}
	return c, nil
}

func (c *Catalog) loadFS(fsys fs.FS, root string) error {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return err
		}
		var a App
		if err := yaml.Unmarshal(b, &a); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		if a.Kind == "" {
			return fmt.Errorf("%s: missing kind", e.Name())
		}
		c.Apps[a.Kind] = &a
	}
	return nil
}

// MatchImage returns the app whose detect.images matches a Docker image ref.
// Patterns are path.Match'ed against every slash-suffix of the untagged image.
func (c *Catalog) MatchImage(image string) *App {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		image = image[:i]
	}
	segs := strings.Split(image, "/")
	for _, a := range c.Apps {
		for _, pat := range a.Detect.Images {
			for i := range segs {
				if ok, _ := path.Match(pat, strings.Join(segs[i:], "/")); ok {
					return a
				}
			}
		}
	}
	return nil
}

// ReadConfig parses an app config file and returns the catalog fields found in it.
func (a *App) ReadConfig(file string) (map[string]string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if a.Config.Format == "regex" { // fields are regexps with one capture group, for odd formats (config.js...)
		for name, expr := range a.Config.Fields {
			re, err := regexp.Compile("(?s)" + expr)
			if err != nil {
				return nil, fmt.Errorf("%s field %s: %w", a.Kind, name, err)
			}
			if m := re.FindSubmatch(b); len(m) > 1 {
				out[name] = strings.TrimSpace(string(m[1]))
			}
		}
		return out, nil
	}
	tree, err := parse(a.Config.Format, b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for name, p := range a.Config.Fields {
		if v, ok := lookup(tree, p); ok {
			out[name] = v
		}
	}
	return out, nil
}

// ReadSecrets runs the catalog's SQLite query against a read-only view of the
// database (immutable: the app may hold it open in WAL mode) and returns every
// key it yields, best first. The caller keeps the one the app accepts.
func (a *App) ReadSecrets(file string) ([]string, error) {
	db, err := sql.Open("sqlite", "file:"+file+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(a.Secret.SQLite)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key sql.NullString
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if key.String != "" {
			keys = append(keys, key.String)
		}
	}
	return keys, rows.Err()
}

func parse(format string, b []byte) (any, error) {
	var tree any
	switch format {
	case "xml":
		return xmlToTree(xml.NewDecoder(strings.NewReader(string(b))))
	case "json":
		return tree, json.Unmarshal(b, &tree)
	case "yaml":
		return tree, yaml.Unmarshal(b, &tree)
	case "ini":
		return iniToTree(b), nil
	}
	return nil, fmt.Errorf("unknown config format %q", format)
}

// xmlToTree: elements -> map[string]any, leaves -> trimmed text.
func xmlToTree(d *xml.Decoder) (any, error) {
	m := map[string]any{}
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			child, err := xmlToTree(d)
			if err != nil {
				return nil, err
			}
			m[t.Name.Local] = child
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if len(m) == 0 {
				return strings.TrimSpace(text.String()), nil
			}
			return m, nil
		}
	}
}

func iniToTree(b []byte) any {
	tree := map[string]any{}
	sec := map[string]any{}
	tree[""] = sec
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line[0] == ';' || line[0] == '#':
		case line[0] == '[' && strings.HasSuffix(line, "]"):
			sec = map[string]any{}
			tree[line[1:len(line)-1]] = sec
		default:
			if k, v, ok := strings.Cut(line, "="); ok {
				sec[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return tree
}

func lookup(tree any, p string) (string, bool) {
	cur := tree
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = m[seg]; !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case map[string]any:
		return "", false
	case float64:
		return fmt.Sprintf("%v", v), true
	default:
		return fmt.Sprint(v), true
	}
}
