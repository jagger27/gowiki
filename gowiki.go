package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/pat"
	"github.com/gorilla/schema"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/mandira"
	"github.com/jmoiron/modl"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/shurcooL/github_flavored_markdown"
)

const (
	DEVELOP = iota
	DEPLOY
)

var MODE = DEPLOY

// templates
type M map[string]interface{}

var templates *mandira.Loader
var t = templates.MustGet

// database
var db *sqlx.DB
var dbm *modl.DbMap

// misc
var cookies *sessions.CookieStore
var decoder = schema.NewDecoder()

// Generate a random string of given length, used for cookie secrets
func GenKey(length int) string {
	alphabet := `ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890 ` + "`" +
		`abcdefghijklmnopqrstuvwxyz~!@#$%^&*()-_+={}[]\\|<,>.?/\"';:`
	bytes := make([]byte, length)
	rand.Read(bytes)
	con := byte(len(alphabet))
	for i, b := range bytes {
		bytes[i] = alphabet[b%con]
	}
	return string(bytes)
}

func MustParse(path string) *mandira.Template {
	var t *mandira.Template
	var err error
	if len(path) > 40 {
		t, err = mandira.ParseString(path)
	} else {
		t, err = mandira.ParseFile(path)
	}
	if err != nil {
		log.Fatal("path: ", path, err)
	}
	return t
}

// return an environment key or a fallback
func environ(key, fallback string) string {
	v := os.Getenv(key)
	if len(v) == 0 {
		return fallback
	}
	return v

}

var opts struct {
	hostname   string
	db         string
	port       string
	debug      bool
	delstatic  bool
	loadstatic bool
}

func main() {
	flag.StringVar(&opts.hostname, "hostname", environ("GOWIKI_HOSTNAME", "localhost"), "hostname to run on")
	flag.StringVar(&opts.port, "port", environ("GOWIKI_PORT", "2222"), "port to run on")
	flag.StringVar(&opts.db, "db", environ("GOWIKI_PATH", "./wiki.db"), "path for wiki db")
	flag.BoolVar(&opts.debug, "debug", len(os.Getenv("GOWIKI_DEVELOP")) > 0, "run with debug mode")
	flag.BoolVar(&opts.delstatic, "del-static", false, "delete db-cached static files")
	flag.BoolVar(&opts.loadstatic, "load-static", false, "reload db-cached static files")
	flag.Parse()

	if opts.debug && opts.delstatic {
		fmt.Printf("Error: cannot specify -debug and -del-static")
		return
	}

	initdb(opts.db)

	if opts.delstatic {
		var files []File
		err := db.Select(&files, "SELECT * FROM file;")
		if err != nil {
			fmt.Printf("Error reading files from db: %s\n", err)
		}
		if err == nil && len(files) > 0 {
			fmt.Printf("Deleting %d cached static files:\n", len(files))
			for _, f := range files {
				fmt.Printf(" > %s\n", f.Path)
			}
		}
		db.MustExec("DELETE FROM file;")
		return
	}

	if opts.loadstatic {
		bootstrap()
		return
	}

	bootstrap()
	// update bundled data with copies from the database
	updateBundle()

	cookies = sessions.NewCookieStore([]byte(cfg.Secret))

	if opts.debug {
		// if we're developing, use /static/ and /templates/
		fmt.Println("Running in development mode without bundled resources.")
		http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static/"))))
		templates = mandira.NewLoader("./templates/", false)
	} else {
		fmt.Println("Running in deployment mode with bundled resources.")
		http.Handle("/static/", http.HandlerFunc(bundleStatic))
		loadTemplatesFromBundle()
	}

	t = templates.MustGet

	// TODO: user/delete && page/delete
	r := pat.New()
	// user management
	r.Get("/users/create", http.HandlerFunc(createUser))
	r.Post("/users/create", http.HandlerFunc(createUser))
	r.Get("/users/login", http.HandlerFunc(login))
	r.Post("/users/login", http.HandlerFunc(login))
	r.Get("/users/logout", http.HandlerFunc(logout))
	r.Get("/users/{id}", http.HandlerFunc(showUser))
	r.Get("/users", http.HandlerFunc(listUsers))
	// page management
	r.Get("/pages/edit{url:.+}", http.HandlerFunc(editPage))
	r.Post("/pages/edit{url:.+}", http.HandlerFunc(editPage))
	r.Get("/pages", http.HandlerFunc(listPages))
	// config
	r.Get("/config/files/{path:.+}", http.HandlerFunc(editFile))
	r.Post("/config/files/{path:.+}", http.HandlerFunc(editFile))
	r.Get("/config/files", http.HandlerFunc(listFiles))
	r.Get("/config", http.HandlerFunc(configWiki))
	r.Post("/config", http.HandlerFunc(configWiki))
	// wiki site
	r.Get("/{url:.*}", http.HandlerFunc(wikipage))

	handler := handlers.LoggingHandler(os.Stdout, r)
	http.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		w.Header().Set("Server", "gowiki")
		w.Header().Set("Date", ts)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		handler.ServeHTTP(w, req)
	}))
	fmt.Println("Listening on " + opts.hostname + ":" + opts.port)
	log.Fatal(http.ListenAndServe(opts.hostname+":"+opts.port, nil))
}

// add back execl because it's been removed from sqlx for a long time
func execl(db sqlx.Execer, q string, args ...interface{}) sql.Result {
	res, err := db.Exec(q, args...)
	if err != nil {
		log.Printf("Error executing %s %#v: %s", q, args, err)
	}
	return res
}

// db

type User struct {
	Id       int
	Username string
	Password string
	Email    string
}

type Page struct {
	Url      string
	Content  string
	Rendered string
	Title    string
	Locked   bool
	OwnedBy  sql.NullInt64
	Modified string
	Links    []string `db:"-"`
}

type File struct {
	Path    string
	Content string
}

type Config struct {
	Key   string
	Value string
}

type Configuration struct {
	Secret         string
	AllowSignups   bool
	AllowAnonEdits bool
	AllowConfigure bool
}

type Crosslink struct {
	From string
	To   string
}

var cfg *Configuration

func InitializeConfig() {
	c1 := Config{Key: "Secret", Value: GenKey(32)}
	c2 := Config{Key: "AllowSignups", Value: "true"}
	c3 := Config{Key: "AllowAnonEdits", Value: "true"}
	c4 := Config{Key: "AllowConfigure", Value: "true"}
	tx, _ := dbm.Begin()
	tx.Insert(&c1, &c2, &c3, &c4)
	tx.Commit()
}

func (c *Configuration) Reload() {
	r := &Config{}
	rows, err := db.Queryx("SELECT * FROM config")
	if err != nil {
		fmt.Println("Error loading configuration: ", err)
		return
	}
	for rows.Next() {
		rows.StructScan(r)
		switch r.Key {
		case "Secret":
			c.Secret = r.Value
		case "AllowSignups":
			c.AllowSignups = r.Value == "true"
		case "AllowAnonEdits":
			c.AllowAnonEdits = r.Value == "true"
		case "AllowConfigure":
			c.AllowConfigure = r.Value == "true"
		}
	}

}

func (c *Configuration) Save() {
	c1 := Config{Key: "Secret", Value: c.Secret}
	c2 := Config{Key: "AllowSignups", Value: fmt.Sprint(c.AllowSignups)}
	c3 := Config{Key: "AllowAnonEdits", Value: fmt.Sprint(c.AllowAnonEdits)}
	c4 := Config{Key: "AllowConfigure", Value: fmt.Sprint(c.AllowConfigure)}
	tx, _ := dbm.Begin()
	tx.Update(&c1, &c2, &c3, &c4)
	tx.Commit()
}

func LoadConfig() *Configuration {
	cfg := &Configuration{}
	cfg.Reload()
	return cfg
}

// renders a page and sets its Rendered content
func (p *Page) Render() string {
	b := github_flavored_markdown.Markdown([]byte(p.Content))
	p.Rendered = string(b)
	p.Rendered, p.Links = MediaWikiParse(p.Rendered)
	return p.Rendered
}

func (p *Page) UpdateCrosslinks() {
	tx := db.MustBegin()
	execl(tx, "DELETE FROM crosslink WHERE `from`=?", p.Url)
	for _, to := range p.Links {
		execl(tx, "INSERT INTO crosslink (`from`, `to`) VALUES (?, ?)", p.Url, to)
	}
	tx.Commit()
}

// Parse out media wiki links, returning the resultant rendered string
// and a slice of page links
func MediaWikiParse(s string) (string, []string) {
	b := []byte(s)
	pat := regexp.MustCompile(`\[\[(?P<link>[^|\]]+)(?:\|(?P<title>[^\]]+))?\]\]`)
	ret := bytes.NewBuffer(make([]byte, 0, len(s)+250))
	links := make([]string, 0, 5)
	start := 0
	for _, r := range pat.FindAllSubmatchIndex(b, -1) {
		var url, title []byte
		begin, end, url1, url2, title1, title2 := r[0], r[1], r[2], r[3], r[4], r[5]
		// allow to escape these in the mediawiki way with ![[foobar]]
		if begin > 0 && b[begin-1] == '!' {
			ret.Write(b[start : begin-1])
			ret.Write(b[begin:end])
			start = end
			continue
		}
		if url1 > 0 {
			url = b[url1:url2]
		}
		if title1 > 0 {
			title = b[title1:title2]
		} else {
			title = url
		}
		if begin > start {
			ret.Write(b[start:begin])
		}

		ret.WriteString(`<a href="/`)
		ret.Write(url)
		ret.WriteString(`">`)
		ret.Write(title)
		ret.WriteString(`</a>`)
		links = append(links, "/"+string(url))
		start = end
	}
	ret.Write(b[start:])
	return ret.String(), links
}

func bootstrap() {
	// initialize the secret key, if necessary
	if len(cfg.Secret) == 0 {
		InitializeConfig()
		cfg.Reload()
		fmt.Println("Auto-created new cookie secret.")
	}

	// initialize the in-db templates and style
	files := make([]*File, 0, 10)
	db.Select(&files, "SELECT * FROM file")
	if len(files) == 0 {
		paths := []string{
			"static/style.css",
			"templates/page.mnd",
			"templates/base.mnd",
		}
		tx, _ := dbm.Begin()
		for _, path := range paths {
			file := &File{Path: path, Content: _bundle[path]}
			tx.Insert(file)
		}
		tx.Commit()
		fmt.Println("Initialized updatable static files.")
	}

	// initialize the index page
	index := &Page{}
	err := db.Get(index, "SELECT * FROM page WHERE url=?", "/")
	if err != nil {
		index := &Page{
			Content:  _bundle["static/default.md"],
			Title:    "Welcome to Gowiki",
			Url:      "/",
			Modified: time.Now().UTC().Format(time.RFC3339Nano),
		}
		index.Render()
		dbm.Insert(index)
		fmt.Println("Auto-created index.")
	}
}

func loadBundle() {
	for k, v := range _bundle {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			fmt.Println(err)
		} else {
			_bundle[k] = string(b)
		}
	}
}

func updateBundle() {
	files := make([]*File, 0, 10)
	db.Select(&files, "SELECT * FROM file")
	for _, f := range files {
		_bundle[f.Path] = f.Content
	}
}

func loadTemplatesFromBundle() {
	templates = mandira.NewLoader("./templates/", false)
	for path, content := range _bundle {
		if mandira.IsTemplate(path) {
			path = strings.TrimPrefix(path, "templates/")
			templates.Add(path, MustParse(content))
		}
	}
	templates.Preload = true
	templates.Loaded = true
	t = templates.MustGet
}

func initdb(path string) {
	var err error

	db, err = sqlx.Connect("sqlite3", path)
	if err != nil {
		log.Fatal("Error: ", err)
	}
	dbm = modl.NewDbMap(db.DB, modl.SqliteDialect{})
	dbm.AddTable(User{}, "user").SetKeys(true, "id")
	dbm.AddTable(Page{}, "page").SetKeys(false, "url")
	dbm.AddTable(Config{}, "config").SetKeys(false, "key")
	dbm.AddTable(File{}, "file").SetKeys(false, "path")
	dbm.AddTable(Crosslink{}, "crosslink").SetKeys(false, "from", "to")
	err = dbm.CreateTablesIfNotExists()

	if err != nil {
		log.Fatal("Database not creatable: ", err)
	}
	// load bundled data
	loadBundle()
	cfg = LoadConfig()
}
