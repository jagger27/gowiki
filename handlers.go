package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"golang.org/x/crypto/bcrypt"
)

func abort(w http.ResponseWriter, status int, body []byte) {
	w.WriteHeader(status)
	w.Write(body)
}

// Handles all non-sepcial wiki pages.
func wikipage(w http.ResponseWriter, req *http.Request) {
	var err error
	page := Page{}
	page.Url = "/" + req.URL.Query().Get(":url")
	err = dbm.Get(&page, page.Url)
	if err != nil {
		w.Write([]byte(t("pagedne.mnd").RenderInLayout(t("base.mnd"), M{"page": page})))
		return
	}
	fromlinks := make([]Crosslink, 0, 10)
	tolinks := make([]Crosslink, 0, 10)
	db.Select(&fromlinks, "SELECT * FROM crosslink WHERE `from`=?", page.Url)
	db.Select(&tolinks, "SELECT * FROM crosslink WHERE `to`=?", page.Url)
	modtime, err := time.Parse(time.RFC3339Nano, page.Modified)
	w.Write([]byte(t("page.mnd").RenderInLayout(t("base.mnd"), M{
		"page": page,
		"PageInfo": M{
			"modified": humanize.Time(modtime),
			"from":     fromlinks,
			"to":       tolinks,
		},
	})))
}

func listUsers(w http.ResponseWriter, req *http.Request) {
	users := []*User{}
	db.Select(&users, "SELECT * FROM user")
	c := t("user.mnd").Render(M{
		"users":  users,
		"config": cfg,
	})
	s := t("base.mnd").Render(M{"content": c})
	w.Write([]byte(s))
}

func createUser(w http.ResponseWriter, req *http.Request) {
	var err error
	user := &User{}

	if !cfg.AllowSignups {
		http.Redirect(w, req, "/", 301)
		return
	}

	if req.Method == "POST" {
		req.ParseForm()
		decoder.Decode(user, req.PostForm)
		hash, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, fmt.Sprintf("bcrypt: %s", err), 500)
			log.Fatal(err)
		}
		user.Password = string(hash)
		u := &User{}
		err = db.Get(u, "SELECT * FROM user WHERE email=?", user.Email)
		if err == nil {
			err = errors.New("User with that email already exists.")
		} else {
			err = dbm.Insert(user)
		}
		if err == nil {
			http.Redirect(w, req, "/users", 303)
			return
		}
	}

	s := t("usercreate.mnd").RenderInLayout(t("base.mnd"), M{
		"error":  err,
		"user":   user,
		"config": cfg,
	})
	w.Write([]byte(s))
}

func showUser(w http.ResponseWriter, req *http.Request) {
	idstr := req.URL.Query().Get(":id")
	id, err := strconv.Atoi(idstr)
	if err != nil {
		fmt.Println(err)
		http.Redirect(w, req, "/users", 301)
		return
	}
	user := &User{}
	err = dbm.Get(user, id)
	if err != nil {
		fmt.Println(err)
		http.Redirect(w, req, "/users", 301)
		return
	}
	pages := []*Page{}
	db.Select(&pages, "SELECT * FROM page WHERE ownedby=?", user.Id)
	w.Write([]byte(t("usershow.mnd").RenderInLayout(t("base.mnd"), M{
		"user":  user,
		"pages": pages,
	})))
}

func login(w http.ResponseWriter, req *http.Request) {
	var err error
	login := User{}
	user := User{}
	if req.Method == "POST" {
		req.ParseForm()
		decoder.Decode(&login, req.PostForm)

		err = db.Get(&user, "SELECT * FROM user WHERE email=?", login.Email)
		err = bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(login.Password))
		if err != nil {
			session, _ := cookies.Get(req, "gowiki-session")
			session.Values["authenticated"] = true
			session.Values["userid"] = user.Id
			session.Save(req, w)
			http.Redirect(w, req, "/", 303)
			return
		}
		if err != nil {
			err = errors.New("Email or Password incorrect.")
		}
	}

	w.Write([]byte(t("login.mnd").RenderInLayout(t("base.mnd"), M{
		"user":   user,
		"error":  err,
		"config": cfg,
	})))
}

func currentuser(req *http.Request) *User {
	session, _ := cookies.Get(req, "gowiki-session")
	if session.Values["authenticated"] != true {
		return nil
	}
	u := &User{}
	err := dbm.Get(u, session.Values["userid"])
	if err != nil {
		return nil
	}
	return u
}

func logout(w http.ResponseWriter, req *http.Request) {
	session, _ := cookies.Get(req, "gowiki-session")
	session.Values["authenticated"] = false
	delete(session.Values, "userid")
	session.Save(req, w)
	http.Redirect(w, req, "/", 302)
}

func listPages(w http.ResponseWriter, req *http.Request) {
	pages := []*Page{}
	db.Select(&pages, "SELECT * FROM page")
	w.Write([]byte(t("listpages.mnd").RenderInLayout(t("base.mnd"), M{"pages": pages})))
}

func checkbox(req *http.Request, key string) bool {
	if v, ok := req.PostForm[key]; ok && len(v) > 0 {
		return true
	}
	return false
}

func editPage(w http.ResponseWriter, req *http.Request) {
	var err error
	canEdit := true
	user := currentuser(req)
	page := &Page{}
	page.Url = req.URL.Query().Get(":url")
	err = dbm.Get(page, page.Url)
	if err == nil && page.Locked && (user == nil || int(page.OwnedBy.Int64) != user.Id) {
		canEdit = false
	}
	if user == nil && !cfg.AllowAnonEdits {
		canEdit = false
		err = nil
	}
	if req.Method == "POST" && canEdit {
		req.ParseForm()
		decoder.Decode(page, req.PostForm)
		// gorilla doesn't really handle the boolean/checkbox here well
		page.Locked = checkbox(req, "Locked")
		if page.Locked {
			page.OwnedBy.Int64 = int64(user.Id)
			page.OwnedBy.Valid = true
		} else {
			page.OwnedBy.Valid = false
		}
		page.Modified = time.Now().UTC().Format(time.RFC3339Nano)
		page.Render()
		if err == nil {
			page.UpdateCrosslinks()
			_, err = dbm.Update(page)
		} else {
			err = dbm.Insert(page)
			if err != nil {
				page.UpdateCrosslinks()
			}
		}
	} else {
		err = dbm.Get(page, page.Url)
	}
	owner := User{}
	if !canEdit && user != nil && page.OwnedBy.Int64 > 0 {
		dbm.Get(&owner, page.OwnedBy)
	}

	if err == sql.ErrNoRows {
		err = nil
	}

	w.Write([]byte(t("editpage.mnd").RenderInLayout(t("base.mnd"), M{
		"page":    page,
		"error":   err,
		"user":    user,
		"owner":   owner,
		"config":  cfg,
		"canEdit": canEdit,
	})))
}

func configWiki(w http.ResponseWriter, req *http.Request) {
	//var err error
	canEdit := true
	user := currentuser(req)
	if user == nil {
		canEdit = false
	}
	if !cfg.AllowConfigure && canEdit && user.Id != 1 {
		canEdit = false
	}
	if req.Method == "POST" && canEdit {
		req.ParseForm()
		cfg.AllowAnonEdits = checkbox(req, "AllowAnonEdits")
		cfg.AllowSignups = checkbox(req, "AllowSignups")
		cfg.AllowConfigure = checkbox(req, "AllowConfigure")
		cfg.Save()
	}

	w.Write([]byte(t("config.mnd").RenderInLayout(t("base.mnd"), M{
		"user":    user,
		"config":  cfg,
		"canEdit": canEdit,
	})))
}

func listFiles(w http.ResponseWriter, req *http.Request) {
	files := make([]*File, 0, 10)
	db.Select(&files, "SELECT * FROM file")
	w.Write([]byte(t("listfiles.mnd").RenderInLayout(t("base.mnd"), M{
		"files": files,
	})))
}

func editFile(w http.ResponseWriter, req *http.Request) {
	var err error

	canEdit := true
	user := currentuser(req)
	if user == nil {
		canEdit = false
	}
	if !cfg.AllowConfigure && canEdit && user.Id != 1 {
		canEdit = false
	}

	path := req.URL.Query().Get(":path")
	file := &File{}
	dbm.Get(file, path)
	if req.Method == "POST" && canEdit {
		req.ParseForm()
		decoder.Decode(file, req.PostForm)
		_, err = dbm.Update(file)
		/* if that update went well, update the in-memory bundle to that content */
		if err == nil {
			_bundle[file.Path] = file.Content
		}
	}

	w.Write([]byte(t("editfile.mnd").RenderInLayout(t("base.mnd"), M{
		"file":    file,
		"canEdit": canEdit,
		"config":  cfg,
		"error":   err,
	})))

}

func bundleStatic(w http.ResponseWriter, req *http.Request) {
	f, ok := _bundle[strings.TrimLeft(req.URL.Path, "/")]
	if ok {
		w.Write([]byte(f))
	} else {
		http.NotFound(w, req)
	}
}
